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

// Two clusters: a single-node MySQL with maintenance pending, and a three-node
// PostgreSQL that is still being created.
const databasesJSON = `{"databases":[` +
	`{"id":"1","name":"main","engine":"mysql","version":"8","num_nodes":1,"size":"db-2vcpu-4gb",` +
	`"region":"fra1","status":"online","storage_size_mib":102400,` +
	`"maintenance_window":{"day":"sunday","hour":"03:00:00","pending":true}},` +
	`{"id":"2","name":"reports","engine":"pg","version":"16","num_nodes":3,"size":"db-2vcpu-4gb",` +
	`"region":"ams3","status":"creating","storage_size_mib":61440,` +
	`"maintenance_window":{"day":"monday","hour":"04:00:00","pending":false}}` +
	`],"meta":{"total":2}}`

const databaseMetrics = `
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
# HELP digitalocean_database_storage_bytes Storage allocated to the cluster.
# TYPE digitalocean_database_storage_bytes gauge
digitalocean_database_storage_bytes{id="1",name="main",region="fra1"} 107374182400
digitalocean_database_storage_bytes{id="2",name="reports",region="ams3"} 64424509440
`

// newTestCollector wires a collector to a fake DigitalOcean API.
func newTestCollector(t *testing.T, handler http.HandlerFunc) *databases.Collector {
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
	return databases.New(client, nil)
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v2/databases" {
		_, _ = w.Write([]byte(databasesJSON))
		return
	}
	w.WriteHeader(http.StatusNotFound)
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

	ch := make(chan *prometheus.Desc, 8)
	c.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}
	if want := 4; count != want {
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
