// Package ratelimit paces HTTP requests to the same origin across the process.
package ratelimit

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrDeferred = errors.New("HTTP rate limit extends beyond the request deadline")

const (
	ExternalInterval = time.Second
	SiloInterval     = 500 * time.Millisecond
	UserAgent        = "silo-theme-plugins (+https://github.com/crowquillx/silo-theme-songs)"
)

type originState struct {
	mu       sync.Mutex
	next     time.Time
	last     time.Time
	interval time.Duration
	failures int
}

var origins sync.Map // map[scheme://host:port]*originState; retained across client reconfiguration

type transport struct {
	base     http.RoundTripper
	interval time.Duration
}

// Wrap applies a minimum interval between request starts to each origin.
// A longer interval or cooldown learned by another wrapper always wins.
// HTTP clients invoke this transport for every redirect hop too.
func Wrap(base http.RoundTripper, interval time.Duration) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if interval < 0 {
		interval = 0
	}
	if wrapped, ok := base.(*transport); ok {
		interval = max(interval, wrapped.interval)
		base = wrapped.base
	}
	return &transport{base: base, interval: interval}
}

func key(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + host + ":" + port
}

func state(u *url.URL) *originState {
	k := key(u)
	actual, _ := origins.LoadOrStore(k, &originState{})
	return actual.(*originState)
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	s := state(req.URL)
	for {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if t.interval > s.interval {
			s.interval = t.interval
			s.next = maxTime(s.next, s.last.Add(s.interval))
		}
		now := time.Now()
		wait := s.next.Sub(now)
		if wait <= 0 {
			s.last = now
			s.next = now.Add(s.interval)
			s.mu.Unlock()
			break
		}
		s.mu.Unlock()
		if deadline, ok := req.Context().Deadline(); ok && !now.Add(wait).Before(deadline) {
			return nil, ErrDeferred
		}
		timer := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	s.observe(resp)
	return resp, nil
}

func (s *originState) observe(resp *http.Response) {
	code := resp.StatusCode
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == http.StatusTooManyRequests || code >= 500 || code == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		s.failures++
		wait := fallback(code, s.failures)
		if headerWait, ok := RetryAfter(resp.Header.Get("Retry-After")); ok {
			wait = headerWait
		}
		if until, ok := resetTime(resp.Header); ok {
			wait = max(wait, time.Until(until))
		}
		s.next = maxTime(s.next, time.Now().Add(wait))
		return
	}
	s.failures = 0
	if until, ok := resetTime(resp.Header); ok {
		s.next = maxTime(s.next, until)
	} else if strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0" {
		// Some services provide reset headers only after rejecting a request.
		// Stop at the last allowed response instead of spending another request.
		s.next = maxTime(s.next, time.Now().Add(time.Minute))
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func fallback(status, failures int) time.Duration {
	if status == http.StatusTooManyRequests || status == http.StatusForbidden {
		return time.Minute
	}
	if failures > 6 {
		failures = 6
	}
	base := time.Second << (failures - 1)
	return base + time.Duration(rand.Int63n(int64(base/4)+1))
}

// RetryAfter accepts delta seconds and HTTP dates without shortening the wait.
func RetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		// Saturate malformed huge values; the caller will defer before requesting.
		if seconds > uint64((1<<63-1)/int64(time.Second)) {
			return time.Duration(1<<63 - 1), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if strings.Trim(value, "0123456789") == "" {
		return time.Duration(1<<63 - 1), true
	}
	if until, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(until)), true
	}
	return 0, false
}

func resetTime(h http.Header) (time.Time, bool) {
	if strings.TrimSpace(h.Get("X-RateLimit-Remaining")) != "0" {
		return time.Time{}, false
	}
	value := strings.TrimSpace(h.Get("X-RateLimit-Reset"))
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	until := time.Unix(seconds, 0)
	if seconds > 1e12 { // AnimeThemes documents Unix milliseconds.
		until = time.UnixMilli(seconds)
	}
	return until, until.After(time.Now())
}

// Waiting returns whether the current origin cooldown exceeds the context budget.
// Callers can report a retryable service error without sending another request.
func Waiting(ctx context.Context, u *url.URL) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	s := state(u)
	s.mu.Lock()
	next := s.next
	s.mu.Unlock()
	return !next.Before(deadline)
}
