package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/bcrypt"

	"github.com/kozaktomas/digitalocean_exporter/internal/config"
)

// TLS and basic auth come from the exporter toolkit, but handing it the
// configuration file is the exporter's own doing, and nothing else here would
// notice if the file stopped reaching it. An exporter deployed with
// credentials that quietly serves its metrics to anyone is the failure this
// catches — and it is one that nothing about a working scrape would show.
func TestServeWithTLSAndBasicAuth(t *testing.T) {
	// The credentials this test invents for the server it starts and stops
	// itself; gosec reads any constant pair shaped like this as a leaked one.
	const user, password = "prometheus", "an unguessable one" //nolint:gosec // not a real credential.

	dir := t.TempDir()
	certFile, keyFile := writeSelfSignedCertificate(t, dir)
	webConfig := writeWebConfig(t, dir, certFile, keyFile, user, password)

	cfg := &config.Config{ListenAddress: freeAddress(t), WebConfigFile: webConfig}
	handler, err := newHandler(prometheus.NewRegistry(), &stubReadiness{}, discardLogger())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfg, handler, discardLogger()) }()

	client := trustingClient(t, certFile)
	url := "https://" + cfg.ListenAddress + "/metrics"
	waitForTLS(t, client, url)

	status, _, err := fetch(t, client, url, "", "")
	if err != nil {
		t.Fatalf("unauthenticated request: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status without credentials = %d, want %d", status, http.StatusUnauthorized)
	}

	status, _, err = fetch(t, client, url, user, "the wrong one")
	if err != nil {
		t.Fatalf("request with a wrong password: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status with a wrong password = %d, want %d", status, http.StatusUnauthorized)
	}

	status, body, err := fetch(t, client, url, user, password)
	if err != nil {
		t.Fatalf("authenticated request: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status with credentials = %d, want %d", status, http.StatusOK)
	}
	if !strings.Contains(body, "promhttp_metric_handler_errors_total") {
		t.Errorf("the exposition looks nothing like /metrics:\n%s", body)
	}

	assertPlainHTTPIsRefused(t, client, "http://"+cfg.ListenAddress+"/metrics")

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within its shutdown timeout")
	}
}

// assertPlainHTTPIsRefused checks that the same port serves nothing over plain
// HTTP: a scrape that was never given the https scheme must fail loudly rather
// than collect metrics the configuration meant to protect.
func assertPlainHTTPIsRefused(t *testing.T, client *http.Client, url string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := client.Do(req)
	if err != nil {
		// A refused handshake is a refusal like any other.
		return
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusOK {
		t.Error("/metrics answered a plain HTTP request with 200, so TLS is not in force")
	}
}

// fetch performs one request, with basic auth credentials when a user is
// given, and returns its status and body. It also insists that the answer
// arrived over TLS: a server that had stopped offering it would otherwise
// satisfy every assertion about the credentials.
func fetch(t *testing.T, client *http.Client, url, user, password string) (int, string, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}

	response, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = response.Body.Close() }()

	if response.TLS == nil {
		t.Error("the response did not arrive over TLS")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return response.StatusCode, string(body), nil
}

// waitForTLS blocks until the server answers over TLS, so the assertions that
// follow cannot outrun the listener and read a connection refused as a
// misconfiguration.
func waitForTLS(t *testing.T, client *http.Client, url string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		response, err := client.Do(req)
		if err == nil {
			_ = response.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the server never served TLS")
}

// writeWebConfig writes the exporter-toolkit configuration that turns TLS and
// basic auth on, and returns its path.
func writeWebConfig(t *testing.T, dir, certFile, keyFile, user, password string) string {
	t.Helper()

	// The minimum cost keeps the test fast. The toolkit only ever compares a
	// password against the hash, so what the hash cost to make is beside the
	// point of what is being tested.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash the password: %v", err)
	}

	path := filepath.Join(dir, "web-config.yml")
	contents := fmt.Sprintf("tls_server_config:\n  cert_file: %s\n  key_file: %s\n"+
		"basic_auth_users:\n  %s: %s\n", certFile, keyFile, user, hash)
	writeFile(t, path, []byte(contents))
	return path
}

// trustingClient returns an HTTPS client that trusts the generated certificate
// and nothing else, which is what makes a successful request proof that the
// server presented it.
func trustingClient(t *testing.T, certFile string) *http.Client {
	t.Helper()

	encoded, err := os.ReadFile(certFile) //nolint:gosec // the path is the test's own temporary directory.
	if err != nil {
		t.Fatalf("read the certificate: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(encoded) {
		t.Fatal("the generated certificate is not a usable PEM")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// writeSelfSignedCertificate generates a certificate for the loopback address
// and writes it, with its key, into dir. Generating one keeps the test
// hermetic, and stops a committed certificate from expiring underneath it one
// day for reasons that have nothing to do with the exporter.
func writeSelfSignedCertificate(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "digitalocean-exporter-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}

	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

// writeFile puts contents in path or fails the test.
func writeFile(t *testing.T, path string, contents []byte) {
	t.Helper()

	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
