package doclient

import (
	"net/http"
	"testing"
	"time"
)

// The backoff is worth testing without waiting for it: a Retry-After is either
// a number of seconds or an HTTP date, and a rejection that carries neither
// still has to be retried eventually.
func TestBackoffPrefersTheServersOwnRetryAfter(t *testing.T) {
	future := time.Now().Add(4 * time.Second).UTC().Format(http.TimeFormat)

	for name, tc := range map[string]struct {
		header  string
		attempt int
		want    time.Duration
		// tolerance covers the clock moving while an HTTP date is parsed.
		tolerance time.Duration
	}{
		"seconds":            {header: "2", attempt: 1, want: 2 * time.Second},
		"zero seconds":       {header: "0", attempt: 1, want: 0},
		"a moment in a past": {header: "Mon, 02 Jan 2006 15:04:05 GMT", attempt: 1, want: 0},
		"an http date":       {header: future, attempt: 1, want: 4 * time.Second, tolerance: time.Second},
		"capped":             {header: "3600", attempt: 1, want: maxBackoff},
		"no header":          {header: "", attempt: 1, want: baseBackoff},
		"no header, doubled": {header: "", attempt: 3, want: 4 * baseBackoff},
		"no header, capped":  {header: "", attempt: 9, want: maxBackoff},
		"unparseable":        {header: "soon", attempt: 1, want: baseBackoff},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tc.header != "" {
				resp.Header.Set("Retry-After", tc.header)
			}

			got := backoff(resp, tc.attempt)
			if diff := got - tc.want; diff < -tc.tolerance || diff > tc.tolerance {
				t.Errorf("backoff = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryableStatuses(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusOK:                  false,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusNotImplemented:      false,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		http.StatusGatewayTimeout:      true,
	} {
		if got := retryable(status); got != want {
			t.Errorf("retryable(%d) = %v, want %v", status, got, want)
		}
	}
}
