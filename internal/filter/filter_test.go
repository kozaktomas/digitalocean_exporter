package filter_test

import (
	"testing"

	"github.com/kozaktomas/digitalocean_exporter/internal/filter"
)

func TestMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tags    []string
		regions []string
		resTags []string
		region  string
		want    bool
	}{
		{
			name:    "empty filter matches everything",
			resTags: []string{"prod"}, region: "fra1", want: true,
		},
		{
			name:    "empty filter matches a resource without tags",
			resTags: nil, region: "fra1", want: true,
		},
		{
			name: "tag filter matches a resource carrying the tag",
			tags: []string{"prod"}, resTags: []string{"prod", "web"}, region: "fra1", want: true,
		},
		{
			name: "tag filter matches on any one of its tags",
			tags: []string{"prod", "staging"}, resTags: []string{"staging"}, region: "fra1", want: true,
		},
		{
			name: "tag filter rejects a resource carrying other tags",
			tags: []string{"prod"}, resTags: []string{"staging"}, region: "fra1", want: false,
		},
		{
			name: "tag filter rejects a resource without tags",
			tags: []string{"prod"}, resTags: nil, region: "fra1", want: false,
		},
		{
			name:    "region filter matches a resource in the region",
			regions: []string{"fra1", "ams3"}, resTags: nil, region: "ams3", want: true,
		},
		{
			name:    "region filter rejects a resource elsewhere",
			regions: []string{"fra1"}, resTags: []string{"prod"}, region: "nyc1", want: false,
		},
		{
			name:    "region filter rejects a resource without a region",
			regions: []string{"fra1"}, resTags: nil, region: "", want: false,
		},
		{
			name: "both conditions must hold: tag alone is not enough",
			tags: []string{"prod"}, regions: []string{"fra1"},
			resTags: []string{"prod"}, region: "nyc1", want: false,
		},
		{
			name: "both conditions must hold: region alone is not enough",
			tags: []string{"prod"}, regions: []string{"fra1"},
			resTags: []string{"staging"}, region: "fra1", want: false,
		},
		{
			name: "both conditions holding matches",
			tags: []string{"prod"}, regions: []string{"fra1"},
			resTags: []string{"prod"}, region: "fra1", want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := filter.New(tt.tags, tt.regions)
			if got := f.Match(tt.resTags, tt.region); got != tt.want {
				t.Errorf("New(%v, %v).Match(%v, %q) = %v, want %v",
					tt.tags, tt.regions, tt.resTags, tt.region, got, tt.want)
			}
		})
	}
}

func TestMatchTagsIgnoresTheRegionCondition(t *testing.T) {
	t.Parallel()

	f := filter.New([]string{"prod"}, []string{"fra1"})
	if !f.MatchTags([]string{"prod"}) {
		t.Error("MatchTags(prod) = false, want true: the region condition must not apply")
	}
	if f.MatchTags([]string{"staging"}) {
		t.Error("MatchTags(staging) = true, want false")
	}
	if f.MatchTags(nil) {
		t.Error("MatchTags(nil) = true, want false: no tags carries none of the asked-for ones")
	}
	if !filter.New(nil, []string{"fra1"}).MatchTags(nil) {
		t.Error("MatchTags with no tag filter = false, want true")
	}
}

func TestSingleTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tags    []string
		regions []string
		want    string
		wantOK  bool
	}{
		{name: "empty filter has no single tag"},
		{name: "one tag and no region", tags: []string{"prod"}, want: "prod", wantOK: true},
		{name: "two tags are not a single tag", tags: []string{"prod", "web"}},
		{name: "a region disqualifies the fast path", tags: []string{"prod"}, regions: []string{"fra1"}},
		{name: "a region alone is not a single tag", regions: []string{"fra1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := filter.New(tt.tags, tt.regions).SingleTag()
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("New(%v, %v).SingleTag() = %q, %v, want %q, %v",
					tt.tags, tt.regions, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
