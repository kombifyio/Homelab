package httpguard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxRetryAfter bounds a pause the control plane asks for, so a wrong
	// header cannot silence a Guard for longer than this.
	maxRetryAfter = time.Hour
	// maxInlineRetryAfter is the longest pause one request waits out before
	// handing the pause to its loop's schedule instead.
	maxInlineRetryAfter = 5 * time.Second
)

// retryAfterError is a control-plane answer that named when to ask again.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

// withRetryAfter attaches the control plane's pause to err; no pause leaves it
// unchanged.
func withRetryAfter(err error, after time.Duration) error {
	if err == nil || after <= 0 {
		return err
	}
	return &retryAfterError{err: err, after: after}
}

// retryAfterOf returns the pause an error carries, zero when it names none.
func retryAfterOf(err error) time.Duration {
	var answer *retryAfterError
	if errors.As(err, &answer) {
		return answer.after
	}
	return 0
}

// parseRetryAfter reads the seconds and HTTP-date forms of Retry-After. An
// absent, invalid or elapsed value names no pause.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, min(date.Sub(now), maxRetryAfter))
	}
	return 0
}

// retryAfterOfResponse is the pause a load answer (429, 503) asks for.
func retryAfterOfResponse(response *http.Response) time.Duration {
	if response == nil || (response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusServiceUnavailable) {
		return 0
	}
	return parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
}

// pauseResults holds result deliveries for the pause the control plane asked
// for; the end of a pause only ever moves later.
func (c *Client) pauseResults(after time.Duration) {
	if after <= 0 {
		return
	}
	until := time.Now().Add(after).UnixNano()
	for {
		current := c.resultsPausedUntil.Load()
		if current >= until || c.resultsPausedUntil.CompareAndSwap(current, until) {
			return
		}
	}
}

// resultPause is how long result deliveries still wait for a pause the
// control plane asked for.
func (c *Client) resultPause() time.Duration {
	return max(0, time.Until(time.Unix(0, c.resultsPausedUntil.Load())))
}
