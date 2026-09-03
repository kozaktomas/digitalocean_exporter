package databases_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/collector/databases"
	"github.com/kozaktomas/digitalocean_exporter/internal/doclient"
)

// Two clusters: a single-node MySQL with maintenance pending, a replica and
// two backups, and a three-node PostgreSQL that is still being created.
const databasesJSON = `{"databases":[` +
	`{"id":"1","name":"main","engine":"mysql","version":"8","num_nodes":1,"size":"db-2vcpu-4gb",` +
	`"region":"fra1","status":"online","storage_size_mib":102400,` +
	`"users":[{"name":"doadmin"},{"name":"app"}],"db_names":["defaultdb","app"],` +
	`"project_id":"p-1","private_network_uuid":"vpc-1",` +
	`"storage_autoscale":{"enabled":true,"threshold_percent":90,"increment_gib":10},` +
	`"maintenance_window":{"day":"sunday","hour":"03:00:00","pending":true}},` +
	`{"id":"2","name":"reports","engine":"pg","version":"16","num_nodes":3,"size":"db-2vcpu-4gb",` +
	`"region":"ams3","status":"creating","storage_size_mib":61440,` +
	`"users":[{"name":"doadmin"}],"db_names":["defaultdb"],` +
	`"project_id":"p-1","private_network_uuid":"vpc-2",` +
	`"maintenance_window":{"day":"monday","hour":"04:00:00","pending":false}}` +
	`],"meta":{"total":2}}`

// The details of cluster 1: one replica in another region, and two backups of
// which the newer one, 2026-08-31T01:00:00Z, is 1788138000 in Unix time.
const (
	replicasJSON = `{"replicas":[` +
		`{"id":"r1","name":"main-replica","region":"ams3","status":"online"}]}`
	backupsJSON = `{"backups":[` +
		`{"created_at":"2026-08-30T01:00:00Z","size_gigabytes":2.5},` +
		`{"created_at":"2026-08-31T01:00:00Z","size_gigabytes":2.6}]}`
)

// databaseMetrics is what the two clusters expose from the list response
// alone, with the detail lookups switched off.
const databaseMetrics = `
# HELP digitalocean_database_cluster_info Always 1. Its labels tie the cluster to its project and its VPC.
# TYPE digitalocean_database_cluster_info gauge
digitalocean_database_cluster_info{id="1",name="main",private_network_uuid="vpc-1",project_id="p-1",region="fra1"} 1
digitalocean_database_cluster_info{id="2",name="reports",private_network_uuid="vpc-2",project_id="p-1",region="ams3"} 1
# HELP digitalocean_database_databases Number of logical databases on the cluster.
# TYPE digitalocean_database_databases gauge
digitalocean_database_databases{id="1",name="main",region="fra1"} 2
digitalocean_database_databases{id="2",name="reports",region="ams3"} 1
# HELP digitalocean_database_maintenance_pending Whether maintenance is pending for the cluster.
# TYPE digitalocean_database_maintenance_pending gauge
digitalocean_database_maintenance_pending{id="1",name="main",region="fra1"} 1
digitalocean_database_maintenance_pending{id="2",name="reports",region="ams3"} 0
# HELP digitalocean_database_nodes Number of nodes in the cluster.
# TYPE digitalocean_database_nodes gauge
digitalocean_database_nodes{engine="mysql",id="1",name="main",region="fra1",size="db-2vcpu-4gb",version="8"} 1
digitalocean_database_nodes{engine="pg",id="2",name="reports",region="ams3",size="db-2vcpu-4gb",version="16"} 3
# HELP digitalocean_database_status Whether the cluster is online.
# TYPE digitalocean_database_status gauge
digitalocean_database_status{engine="mysql",id="1",name="main",region="fra1",size="db-2vcpu-4gb",version="8"} 1
digitalocean_database_status{engine="pg",id="2",name="reports",region="ams3",size="db-2vcpu-4gb",version="16"} 0
# HELP digitalocean_database_storage_autoscale_enabled Whether the cluster grows its own storage before it fills.
# TYPE digitalocean_database_storage_autoscale_enabled gauge
digitalocean_database_storage_autoscale_enabled{id="1",name="main",region="fra1"} 1
digitalocean_database_storage_autoscale_enabled{id="2",name="reports",region="ams3"} 0
# HELP digitalocean_database_storage_bytes Storage allocated to the cluster.
# TYPE digitalocean_database_storage_bytes gauge
digitalocean_database_storage_bytes{id="1",name="main",region="fra1"} 107374182400
digitalocean_database_storage_bytes{id="2",name="reports",region="ams3"} 64424509440
# HELP digitalocean_database_users Number of database users on the cluster.
# TYPE digitalocean_database_users gauge
digitalocean_database_users{id="1",name="main",region="fra1"} 2
digitalocean_database_users{id="2",name="reports",region="ams3"} 1
`

// databaseDetailMetrics adds what the detail lookups answer for cluster 1.
// Cluster 2's lookups fail in the test that uses this, and a cluster whose
// lookup has never succeeded emits no detail series at all.
const databaseDetailMetrics = databaseMetrics + `
# HELP digitalocean_database_last_backup_timestamp_seconds Unix time the newest backup of the cluster was taken.
# TYPE digitalocean_database_last_backup_timestamp_seconds gauge
digitalocean_database_last_backup_timestamp_seconds{id="1",name="main",region="fra1"} 1788138000
# HELP digitalocean_database_replica_status Always 1 for the replica's current status and 0 for every other known one.
# TYPE digitalocean_database_replica_status gauge
digitalocean_database_replica_status{id="1",name="main",region="ams3",replica="main-replica",status="creating"} 0
digitalocean_database_replica_status{id="1",name="main",region="ams3",replica="main-replica",status="forking"} 0
digitalocean_database_replica_status{id="1",name="main",region="ams3",replica="main-replica",status="migrating"} 0
digitalocean_database_replica_status{id="1",name="main",region="ams3",replica="main-replica",status="online"} 1
digitalocean_database_replica_status{id="1",name="main",region="ams3",replica="main-replica",status="resizing"} 0
# HELP digitalocean_database_replicas Number of read-only replicas of the cluster.
# TYPE digitalocean_database_replicas gauge
digitalocean_database_replicas{id="1",name="main",region="fra1"} 1
`

// newCollector wires a collector to a fake DigitalOcean API.
func newCollector(t *testing.T, details bool, handler http.HandlerFunc) *databases.Collector {
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
	return databases.New(client, details, nil)
}

// newTestCollector wires a collector with the detail lookups switched off.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *databases.Collector {
	t.Helper()
	return newCollector(t, false, handler)
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/databases" {
		_, _ = w.Write([]byte(databasesJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// detailsHandler serves the list, the details of cluster 1, and — while fail2
// is set — a server error for the details of cluster 2.
func detailsHandler(fail2 *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/databases":
			_, _ = w.Write([]byte(databasesJSON))
		case "/v2/databases/1/replicas":
			_, _ = w.Write([]byte(replicasJSON))
		case "/v2/databases/1/backups":
			_, _ = w.Write([]byte(backupsJSON))
		case "/v2/databases/2/replicas", "/v2/databases/2/backups":
			if fail2 != nil && fail2.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"replicas":[],"backups":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestCollectAfterRefresh(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(databaseMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// One cluster's detail lookup failing must not fail the refresh or cost the
// clusters whose lookups succeeded. With nothing from an earlier refresh to
// fall back on, the failed cluster emits no detail series at all.
func TestCollectWithDetails(t *testing.T) {
	var fail2 atomic.Bool
	fail2.Store(true)
	c := newCollector(t, true, detailsHandler(&fail2))

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(databaseDetailMetrics)); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// A detail lookup that fails after having succeeded keeps reporting what the
// last successful lookup found, exactly as a failed refresh keeps the previous
// snapshot.
func TestDetailFailureKeepsPreviousDetails(t *testing.T) {
	var fail2 atomic.Bool
	c := newCollector(t, true, detailsHandler(&fail2))

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	fail2.Store(true)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with a failing detail lookup: %v", err)
	}

	const want = `
# HELP digitalocean_database_replicas Number of read-only replicas of the cluster.
# TYPE digitalocean_database_replicas gauge
digitalocean_database_replicas{id="1",name="main",region="fra1"} 1
digitalocean_database_replicas{id="2",name="reports",region="ams3"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_database_replicas"); err != nil {
		t.Errorf("unexpected metrics after a failed detail lookup: %v", err)
	}
}

// Engines without backups — caching clusters, say — answer the backups
// endpoint with a client error. That is an answer, "this cluster has no
// backups", not a failure: the refresh succeeds, the replicas are still
// reported and no backup series appears.
func TestBackupsUnsupportedReadsAsNoBackups(t *testing.T) {
	c := newCollector(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/databases":
			_, _ = w.Write([]byte(databasesJSON))
		case "/v2/databases/1/backups", "/v2/databases/2/backups":
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"id":"precondition_failed","message":"backups are not supported"}`))
		case "/v2/databases/1/replicas":
			_, _ = w.Write([]byte(replicasJSON))
		case "/v2/databases/2/replicas":
			_, _ = w.Write([]byte(`{"replicas":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := testutil.CollectAndCount(c, "digitalocean_database_last_backup_timestamp_seconds"); got != 0 {
		t.Errorf("backup series with backups unsupported = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(c, "digitalocean_database_replicas"); got != 2 {
		t.Errorf("replica count series = %d, want 2", got)
	}
}

// With details switched off the refresh must not spend the two per-cluster
// requests, and no detail series may appear.
func TestDetailsOffCostsNoDetailRequests(t *testing.T) {
	var detailRequests atomic.Int64
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/databases" {
			detailRequests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := detailRequests.Load(); got != 0 {
		t.Errorf("detail requests with details off = %d, want 0", got)
	}
	for _, metric := range []string{
		"digitalocean_database_replicas",
		"digitalocean_database_replica_status",
		"digitalocean_database_last_backup_timestamp_seconds",
	} {
		if got := testutil.CollectAndCount(c, metric); got != 0 {
			t.Errorf("%s series with details off = %d, want 0", metric, got)
		}
	}
}

// A cluster without a maintenance window still has to report the rest of its
// figures rather than taking the whole refresh down with it.
func TestRefreshWithoutMaintenanceWindow(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"databases":[{"id":"1","name":"main","engine":"mysql","version":"8",` +
			`"num_nodes":1,"size":"db-2vcpu-4gb","region":"fra1","status":"online",` +
			`"storage_size_mib":102400}],"meta":{"total":1}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const want = `
# HELP digitalocean_database_maintenance_pending Whether maintenance is pending for the cluster.
# TYPE digitalocean_database_maintenance_pending gauge
digitalocean_database_maintenance_pending{id="1",name="main",region="fra1"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"digitalocean_database_maintenance_pending"); err != nil {
		t.Errorf("unexpected metrics: %v", err)
	}
}

// godo does not expose the pagination links of the database list, so a full
// page is the only signal that another one may follow. An account with more
// clusters than fit on a page must still be reported whole.
func TestRefreshFollowsPages(t *testing.T) {
	const perPage = 200
	page := func(from, count int) string {
		clusters := make([]string, 0, count)
		for i := from; i < from+count; i++ {
			clusters = append(clusters, fmt.Sprintf(`{"id":"%d","name":"db-%d","engine":"mysql","version":"8",`+
				`"num_nodes":1,"size":"db-2vcpu-4gb","region":"fra1","status":"online"}`, i, i))
		}
		return `{"databases":[` + strings.Join(clusters, ",") + `],"meta":{"total":201}}`
	}

	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(page(perPage+1, 1)))
			return
		}
		_, _ = w.Write([]byte(page(1, perPage)))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := testutil.CollectAndCount(c, "digitalocean_database_nodes"); got != perPage+1 {
		t.Errorf("clusters collected = %d, want %d", got, perPage+1)
	}
}

// An account with no managed database is a normal state.
func TestRefreshWithoutDatabasesSucceeds(t *testing.T) {
	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"databases":[],"meta":{"total":0}}`))
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh without databases: %v", err)
	}
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count without databases = %d, want 0", got)
	}
}

func TestCollectBeforeRefreshEmitsNothing(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("metric count before the first refresh = %d, want 0", got)
	}
}

func TestFailedRefreshKeepsPreviousSnapshot(t *testing.T) {
	var fail bool
	c := newTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	fail = true
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(databaseMetrics)); err != nil {
		t.Errorf("unexpected metrics after a failed refresh: %v", err)
	}
}

func TestName(t *testing.T) {
	c := newTestCollector(t, okHandler)
	if got := c.Name(); got != "databases" {
		t.Errorf("Name() = %q, want %q", got, "databases")
	}
}

func TestDescribeCoversEveryMetric(t *testing.T) {
	c := newTestCollector(t, okHandler)

	ch := make(chan *prometheus.Desc, 16)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 11; count != want {
		t.Errorf("Describe sent %d descriptors, want %d", count, want)
	}
}

// The list endpoint does not document paging and godo drops its links, so an
// implementation that ignores the page parameter answers page 2, 3 and every
// page after it with the same full list. The refresh stops at the first cluster
// it has already seen rather than paging until its deadline, which would fail
// every refresh on an account holding a full page of clusters.
func TestRefreshStopsWhenTheListIgnoresThePage(t *testing.T) {
	const perPage = 200
	var requests atomic.Int64

	clusters := make([]string, 0, perPage)
	for i := 1; i <= perPage; i++ {
		clusters = append(clusters, fmt.Sprintf(`{"id":"%d","name":"db-%d","engine":"mysql","version":"8",`+
			`"num_nodes":1,"size":"db-2vcpu-4gb","region":"fra1","status":"online"}`, i, i))
	}
	body := `{"databases":[` + strings.Join(clusters, ",") + `],"meta":{"total":200}}`

	c := newTestCollector(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	// A deadline of its own, so a loop that does not terminate fails this test
	// instead of hanging it until the package times out.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := testutil.CollectAndCount(c, "digitalocean_database_nodes"); got != perPage {
		t.Errorf("clusters collected = %d, want %d", got, perPage)
	}
	// The full first page asks for a second one; the repeat it answers with is
	// where the walk ends.
	if got := requests.Load(); got != 2 {
		t.Errorf("list requests = %d, want 2", got)
	}
}
