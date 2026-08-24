package version_test

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kozaktomas/digitalocean_exporter/internal/version"
)

func TestCollectorEmitsBuildInfo(t *testing.T) {
	version.Version = "1.2.3"
	version.Commit = "abc1234"

	expected := fmt.Sprintf(`
# HELP digitalocean_exporter_build_info Build metadata of the running exporter. Always 1.
# TYPE digitalocean_exporter_build_info gauge
digitalocean_exporter_build_info{commit="abc1234",goversion="%s",version="1.2.3"} 1
`, runtime.Version())

	if err := testutil.CollectAndCompare(version.NewCollector(), strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected metrics: %v", err)
	}
}
