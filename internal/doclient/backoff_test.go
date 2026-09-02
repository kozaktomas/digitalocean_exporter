package doclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// The wait is worth testing without waiting for it: a Retry-After is either a
// number of seconds or an HTTP date, a rejection that carries neither still
// has to be retried eventually, and a rejection that says the hour is spent
// must not be retried at all.
func TestRetryDelayPrefersTheServersOwnRetryAfter(t *testing.T) {
	future := time.Now().Add(4 * time.Second).UTC().Format(http.TimeFormat)

	for name, tc := range map[string]struct {
		status  int
		header  http.Header
		attempt int
		want    time.Duration
		wantOK  bool
		// tolerance covers the clock moving while an HTTP date is parsed.
		tolerance time.Duration
	}{
		"seconds": {
			status: http.StatusTooManyRequests, header: retryAfterHeader("2"),
			attempt: 1, want: 2 * time.Second, wantOK: true,
		},
		"zero seconds": {
			status: http.StatusTooManyRequests, header: retryAfterHeader("0"),
			attempt: 1, want: 0, wantOK: true,
		},
		"a moment in the past": {
			status:  http.StatusTooManyRequests,
			header:  retryAfterHeader("Mon, 02 Jan 2006 15:04:05 GMT"),
			attempt: 1, want: 0, wantOK: true,
		},
		"an http date": {
			status: http.StatusTooManyRequests, header: retryAfterHeader(future),
			attempt: 1, want: 4 * time.Second, wantOK: true, tolerance: time.Second,
		},
		// A long Retry-After is honoured in full: retrying sooner is a
		// rejection bought with a request from the hourly budget.
		"a long wait is not capped": {
			status: http.StatusTooManyRequests, header: retryAfterHeader("45"),
			attempt: 1, want: 45 * time.Second, wantOK: true,
		},
		"no header": {
			status: http.StatusServiceUnavailable, attempt: 1,
			want: baseBackoff, wantOK: true,
		},
		"no header, doubled": {
			status: http.StatusServiceUnavailable, attempt: 3,
			want: 4 * baseBackoff, wantOK: true,
		},
		"no header, capped": {
			status: http.StatusServiceUnavailable, attempt: 9,
			want: maxBackoff, wantOK: true,
		},
		"unparseable": {
			status: http.StatusTooManyRequests, header: retryAfterHeader("soon"),
			attempt: 1, want: baseBackoff, wantOK: true,
		},
		// The hourly limit: nothing frees it up before the hour turns.
		"the hourly limit is not retried": {
			status:  http.StatusTooManyRequests,
			header:  http.Header{"Ratelimit-Remaining": []string{"0"}},
			attempt: 1, wantOK: false,
		},
		// The burst limit reached with the hour spent as well: the header says
		// when the window reopens, so it is still worth the wait.
		"a retry-after outranks a spent budget": {
			status: http.StatusTooManyRequests,
			header: http.Header{
				"Retry-After":         []string{"3"},
				"Ratelimit-Remaining": []string{"0"},
			},
			attempt: 1, want: 3 * time.Second, wantOK: true,
		},
		// The zero belongs to the 429 rule alone; a 500 is transient whatever
		// the budget says.
		"a spent budget does not stop a 5xx": {
			status:  http.StatusInternalServerError,
			header:  http.Header{"Ratelimit-Remaining": []string{"0"}},
			attempt: 1, want: baseBackoff, wantOK: true,
		},
		"a bad token is not retried": {
			status: http.StatusUnauthorized, attempt: 1, wantOK: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			for key, values := range tc.header {
				resp.Header[key] = values
			}

			got, ok := retryDelay(resp, tc.attempt)
			if ok != tc.wantOK {
				t.Fatalf("retryDelay ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if diff := got - tc.want; diff < -tc.tolerance || diff > tc.tolerance {
				t.Errorf("retryDelay = %v, want %v", got, tc.want)
			}
		})
	}
}

// retryAfterHeader builds the header of a response that carries a Retry-After.
func retryAfterHeader(value string) http.Header {
	return http.Header{"Retry-After": []string{value}}
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

// A broken connection is worth another try; a context that is done is not,
// because the caller has already stopped waiting for the answer.
func TestRetryableErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"unexpected eof":    {err: io.ErrUnexpectedEOF, want: true},
		"connection reset":  {err: &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, want: true},
		"dns failure":       {err: &net.DNSError{Err: "no such host", IsNotFound: true}, want: true},
		"cancelled":         {err: context.Canceled, want: false},
		"deadline exceeded": {err: context.DeadlineExceeded, want: false},
		// The limiter reports a done context wrapped; errors.Is sees through it.
		"the limiter's wrap": {err: fmt.Errorf("wait for the API rate limiter: %w", context.Canceled), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := retryableError(tc.err); got != tc.want {
				t.Errorf("retryableError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A wait that outlasts the deadline buys nothing: the retry would be cut short
// before it was made.
func TestFitsWithinTheDeadline(t *testing.T) {
	if !fits(context.Background(), time.Hour) {
		t.Error("a context without a deadline should fit any wait")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if !fits(ctx, time.Millisecond) {
		t.Error("a short wait should fit inside the deadline")
	}
	if fits(ctx, time.Minute) {
		t.Error("a wait past the deadline should not fit")
	}
}
