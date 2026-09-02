package paging_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// newClient wires a godo client to a fake DigitalOcean API.
func newClient(t *testing.T, handler http.HandlerFunc) *godo.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := doclient.New(doclient.Config{
		Token: "token", BaseURL: srv.URL + "/", UserAgent: "test", Timeout: 5 * time.Second,
		// One attempt: retrying a stubbed failure only makes this test sit
		// through the backoff, and the retries have their own test in doclient.
		MaxAttempts: 1, Metrics: doclient.NewMetrics(prometheus.NewRegistry()),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// dropletsPage renders one page of the droplet list. prev and next are the
// page numbers the response links back and on to, zero for neither; godo reads
// the page it is on from the previous one, as the API's own responses let it.
func dropletsPage(prev, next int, ids ...int) string {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, fmt.Sprintf(`{"id":%d,"name":"d-%d"}`, id, id))
	}
	var pages []string
	if prev > 0 {
		pages = append(pages, fmt.Sprintf(`"prev":"https://api.digitalocean.com/v2/droplets?page=%d"`, prev))
	}
	if next > 0 {
		pages = append(pages, fmt.Sprintf(`"next":"https://api.digitalocean.com/v2/droplets?page=%d"`, next))
	}
	return fmt.Sprintf(`{"droplets":[%s],"links":{"pages":{%s}},"meta":{"total":%d}}`,
		strings.Join(items, ","), strings.Join(pages, ","), len(ids))
}

// dropletID is the key every test here deduplicates on.
func dropletID(d godo.Droplet) int { return d.ID }

// ids names the droplets a walk returned, in order.
func ids(droplets []godo.Droplet) []int {
	out := make([]int, 0, len(droplets))
	for _, d := range droplets {
		out = append(out, d.ID)
	}
	return out
}

// A list longer than one page is walked to its end, in order, and every page
// asks for the largest page the API allows.
func TestAllFollowsEveryPage(t *testing.T) {
	var perPage []string
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		perPage = append(perPage, r.URL.Query().Get("per_page"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "2":
			_, _ = w.Write([]byte(dropletsPage(1, 3, 3, 4)))
		case "3":
			_, _ = w.Write([]byte(dropletsPage(2, 0, 5)))
		default:
			_, _ = w.Write([]byte(dropletsPage(0, 2, 1, 2)))
		}
	})

	got, err := paging.All(context.Background(), nil, "droplets", dropletID, client.Droplets.List)
	if err != nil {
		t.Fatalf("all: %v", err)
	}

	if want := []int{1, 2, 3, 4, 5}; !slices.Equal(ids(got), want) {
		t.Errorf("droplets = %v, want %v", ids(got), want)
	}
	if len(perPage) != 3 {
		t.Fatalf("requests = %d, want 3", len(perPage))
	}
	for i, got := range perPage {
		if want := fmt.Sprint(paging.PerPage); got != want {
			t.Errorf("request %d asked for per_page=%q, want %q", i+1, got, want)
		}
	}
}

// An item that appears on two pages — the list shifted between the requests —
// is kept once, at the position it was first seen.
func TestAllDropsDuplicatesAcrossPages(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(dropletsPage(1, 0, 2, 3)))
			return
		}
		_, _ = w.Write([]byte(dropletsPage(0, 2, 1, 2)))
	})

	got, err := paging.All(context.Background(), nil, "droplets", dropletID, client.Droplets.List)
	if err != nil {
		t.Fatalf("all: %v", err)
	}

	if want := []int{1, 2, 3}; !slices.Equal(ids(got), want) {
		t.Errorf("droplets = %v, want %v", ids(got), want)
	}
}

// A duplicate within one page is dropped too: the same guard covers it, and
// two identical items on one page break a scrape just as badly.
func TestAllDropsDuplicatesWithinAPage(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dropletsPage(0, 0, 7, 7)))
	})

	got, err := paging.All(context.Background(), nil, "droplets", dropletID, client.Droplets.List)
	if err != nil {
		t.Fatalf("all: %v", err)
	}

	if want := []int{7}; !slices.Equal(ids(got), want) {
		t.Errorf("droplets = %v, want %v", ids(got), want)
	}
}

// The walk stops at the first failing page and returns nothing: half an
// account is worse than no account, because the missing half reads as
// destroyed.
func TestAllReturnsNothingWhenAPageFails(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dropletsPage(0, 2, 1)))
	})

	got, err := paging.All(context.Background(), nil, "droplets", dropletID, client.Droplets.List)
	if err == nil {
		t.Fatal("expected the failing second page to fail the walk")
	}
	if !strings.Contains(err.Error(), "list droplets") {
		t.Errorf("error = %q, want it to name the resource", err)
	}
	if got != nil {
		t.Errorf("droplets = %v, want none", ids(got))
	}
}

// A cancelled context ends the walk before the next request is made, rather
// than spending a slot of the rate limit on a request that cannot succeed.
func TestAllStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var requests int

	list := func(_ context.Context, opts *godo.ListOptions) ([]godo.Droplet, *godo.Response, error) {
		requests++
		cancel()
		resp := &godo.Response{Links: &godo.Links{Pages: &godo.Pages{
			Next: "https://api.digitalocean.com/v2/droplets?page=2",
		}}}
		return []godo.Droplet{{ID: opts.Page + 1}}, resp, nil
	}

	got, err := paging.All(ctx, nil, "droplets", dropletID, list)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got != nil {
		t.Errorf("droplets = %v, want none", ids(got))
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
}

// A context already cancelled when the walk starts makes no request at all.
func TestAllMakesNoRequestWhenAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var requests int
	list := func(_ context.Context, _ *godo.ListOptions) ([]godo.Droplet, *godo.Response, error) {
		requests++
		return nil, nil, nil
	}

	if _, err := paging.All(ctx, nil, "droplets", dropletID, list); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0", requests)
	}
}

// A response without pagination links — an endpoint that does not paginate, or
// a stub that omits them — is the last page.
func TestAllStopsWithoutLinks(t *testing.T) {
	var requests int
	list := func(_ context.Context, _ *godo.ListOptions) ([]godo.Droplet, *godo.Response, error) {
		requests++
		return []godo.Droplet{{ID: 1}}, &godo.Response{}, nil
	}

	got, err := paging.All(context.Background(), nil, "droplets", dropletID, list)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if want := []int{1}; !slices.Equal(ids(got), want) {
		t.Errorf("droplets = %v, want %v", ids(got), want)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
}

// An account with nothing in it is a walk that returns nothing and no error.
func TestAllOnEmptyList(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dropletsPage(0, 0)))
	})

	got, err := paging.All(context.Background(), nil, "droplets", dropletID, client.Droplets.List)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("droplets = %v, want none", ids(got))
	}
}

// The debug line names the resource and the duplicate that was dropped, so an
// account whose list keeps shifting can be recognised in the log rather than
// guessed at from a scrape that no longer fails.
func TestAllLogsTheDuplicateItDrops(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	list := func(_ context.Context, opts *godo.ListOptions) ([]godo.Droplet, *godo.Response, error) {
		if opts.Page == 2 {
			return []godo.Droplet{{ID: 1}}, &godo.Response{}, nil
		}
		resp := &godo.Response{Links: &godo.Links{Pages: &godo.Pages{
			Next: "https://api.digitalocean.com/v2/droplets?page=2",
		}}}
		return []godo.Droplet{{ID: 1}}, resp, nil
	}

	got, err := paging.All(context.Background(), logger, "droplets", dropletID, list)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if want := []int{1}; !slices.Equal(ids(got), want) {
		t.Fatalf("droplets = %v, want %v", ids(got), want)
	}

	line := buf.String()
	for _, want := range []string{"level=DEBUG", "resource=droplets", "key=1", "page=2"} {
		if !strings.Contains(line, want) {
			t.Errorf("debug line %q does not contain %q", line, want)
		}
	}
}
