// Package filter narrows what the resource collectors report to a slice of
// the account: only the resources carrying one of a set of tags, lying in one
// of a set of regions, or both. An account shared between teams, or one with
// a noisy staging region, runs the exporter with a filter and scrapes only
// its own resources.
//
// The filter is applied client-side, after a collector has listed its
// resources, so it changes what is reported without changing what the listing
// costs. Every affected collector holds the same Filter and asks it the same
// question, which is what keeps "matches the filter" meaning one thing across
// the whole exposition.
package filter

// Filter selects resources by tag and by region. The zero value is the empty
// filter, which matches everything — an exporter configured with no filter
// reports the whole account, exactly as it did before filters existed.
type Filter struct {
	tags    map[string]struct{}
	regions map[string]struct{}
}

// New builds a Filter matching the resources that carry at least one of tags
// (all of them, when tags is empty) and lie in one of regions (anywhere, when
// regions is empty).
func New(tags, regions []string) Filter {
	return Filter{tags: asSet(tags), regions: asSet(regions)}
}

// asSet turns a list into a membership set, or nil for an empty list so the
// zero Filter and New(nil, nil) behave identically.
func asSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}

// Match reports whether a resource with the given tags, in the given region,
// passes the filter: both the tag condition and the region condition must
// hold, and a condition nobody configured holds for everything. A resource
// without tags fails a tag filter — it carries none of the asked-for tags —
// and a resource the API reports without a region fails a region filter the
// same way.
func (f Filter) Match(tags []string, region string) bool {
	return f.MatchTags(tags) && f.matchRegion(region)
}

// MatchTags reports whether a resource carrying the given tags passes the tag
// condition alone. It exists for the firewalls collector: a cloud firewall
// has no region, so holding it against the region condition would empty the
// collector whenever a region filter is set, and a firewall is region-less by
// nature rather than by omission.
func (f Filter) MatchTags(tags []string) bool {
	if f.tags == nil {
		return true
	}
	for _, tag := range tags {
		if _, ok := f.tags[tag]; ok {
			return true
		}
	}
	return false
}

// matchRegion reports whether a resource in the given region passes the
// region condition alone.
func (f Filter) matchRegion(region string) bool {
	if f.regions == nil {
		return true
	}
	_, ok := f.regions[region]
	return ok
}

// SingleTag returns the filter's one tag when the filter is exactly one tag
// and no region — the one shape the DigitalOcean API can apply server-side,
// through the tag-scoped droplet listing. The droplet collectors use it to
// let the API do the filtering and spend fewer requests; every other shape
// filters client-side.
func (f Filter) SingleTag() (string, bool) {
	if len(f.tags) != 1 || f.regions != nil {
		return "", false
	}
	for tag := range f.tags {
		return tag, true
	}
	return "", false
}
