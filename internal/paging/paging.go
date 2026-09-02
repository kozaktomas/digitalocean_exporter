// Package paging walks the page-numbered lists of the DigitalOcean API.
//
// Every list endpoint the collectors read is paginated the same way — a page
// number, a page size and a `links.pages.next` that stops appearing on the last
// page — so every collector that reads one used to carry the same loop. They
// share this one instead.
//
// The helper deduplicates by a key the caller supplies, which the hand-written
// loops did not. A list can shift between two page requests: a droplet created
// or destroyed while the pages are being read moves an item across the page
// boundary, and the same item then arrives on both pages. Two snapshot entries
// for one resource become two series with identical labels, which Prometheus
// rejects — the whole scrape fails, not just the duplicated metric. Keeping the
// first occurrence costs one map and makes that impossible.
package paging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/digitalocean/godo"
)

// PerPage is how many items one page request asks for, which is the most the
// API allows. A larger page is fewer requests against a rate limit that counts
// them.
const PerPage = 200

// ListFunc reads one page of a list. It is godo's list signature with the
// endpoint and its arguments bound, so a caller with options of its own — the
// volume list takes a struct wrapping godo.ListOptions — can adapt in a
// closure.
type ListFunc[T any] func(ctx context.Context, opts *godo.ListOptions) ([]T, *godo.Response, error)

// All reads every page of list and returns the items in the order the API gave
// them, with any item whose key was seen before dropped.
//
// what names the resource for the error messages and the debug line: it is the
// plural a reader would expect after "list", such as "droplets". A nil logger
// discards the debug lines.
//
// The first failure ends the walk and nothing is returned, because a collector
// that swapped half an account into its snapshot would report the other half as
// having been destroyed.
func All[T any, K comparable](
	ctx context.Context, logger *slog.Logger, what string, key func(T) K, list ListFunc[T],
) ([]T, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	opts := &godo.ListOptions{PerPage: PerPage}
	var out []T
	seen := make(map[K]struct{})

	for {
		// A cancelled refresh stops here rather than after one more page. The
		// request would fail on the same context, but only after the transport
		// has spent a slot of the rate limit on it.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("list %s: %w", what, err)
		}

		page, resp, err := list(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", what, err)
		}
		out = keepNew(out, page, seen, key, func(k K) {
			logger.Debug("dropped a duplicate from a paginated list",
				"resource", what, "key", fmt.Sprint(k), "page", opts.Page)
		})

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return out, nil
		}
		current, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, fmt.Errorf("next page of %s: %w", what, err)
		}
		opts.Page = current + 1
	}
}

// keepNew appends every item of page whose key seen does not already hold to
// out, and records the keys it kept. dropped is called with the key of each
// repeat instead, which is what makes the duplicate visible in the log.
func keepNew[T any, K comparable](out, page []T, seen map[K]struct{}, key func(T) K, dropped func(K)) []T {
	for _, item := range page {
		k := key(item)
		if _, dup := seen[k]; dup {
			dropped(k)
			continue
		}
		seen[k] = struct{}{}
		out = append(out, item)
	}
	return out
}
