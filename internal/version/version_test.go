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

func TestString(t *testing.T) {
	// The build metadata are package-level variables stamped at link time, so
	// the test restores whatever the rest of the package expects to find.
	oldVersion, oldCommit := version.Version, version.Commit
	t.Cleanup(func() {
		version.Version, version.Commit = oldVersion, oldCommit
	})

	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{
			name:    "a stamped release build",
			version: "1.2.3",
			commit:  "abc1234",
			want:    "digitalocean_exporter, version 1.2.3 (commit abc1234, " + runtime.Version() + ")",
		},
		{
			name:    "an unstamped go build reports the defaults",
			version: "dev",
			commit:  "none",
			want:    "digitalocean_exporter, version dev (commit none, " + runtime.Version() + ")",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version.Version, version.Commit = tt.version, tt.commit
			if got := version.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
