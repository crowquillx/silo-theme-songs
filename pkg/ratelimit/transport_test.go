package ratelimit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse("https://" + strings.ReplaceAll(t.Name(), "/", "-") + ".invalid/path")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { origins.Delete(key(u)) })
	return u
}

func response(code int, header http.Header) *http.Response {
	return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader(""))}
}

func TestCooldownSurvivesNewWrapperAndLastAttempt(t *testing.T) {
	u := testURL(t)
	requests := 0
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusTooManyRequests, http.Header{"Retry-After": {"600"}}), nil
	})
	first := Wrap(base, time.Millisecond)
	if _, err := first.RoundTrip((&http.Request{Method: "GET", URL: u}).WithContext(context.Background())); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := Wrap(base, time.Millisecond).RoundTrip((&http.Request{Method: "GET", URL: u}).WithContext(ctx))
	if !errors.Is(err, ErrDeferred) || requests != 1 {
		t.Fatalf("next client sent a throttled request: requests=%d err=%v", requests, err)
	}
}

func TestNoHeaderAndResetCooldown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
	}{
		{"missing retry header", http.StatusTooManyRequests, nil},
		{"exhausted without reset", http.StatusOK, http.Header{"X-Ratelimit-Remaining": {"0"}}},
		{"exhausted reset", http.StatusOK, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"9999999999"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := testURL(t)
			requests := 0
			base := roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return response(tc.status, tc.header), nil
			})
			tr := Wrap(base, time.Millisecond)
			_, err := tr.RoundTrip((&http.Request{Method: "GET", URL: u}).WithContext(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err = tr.RoundTrip((&http.Request{Method: "GET", URL: u}).WithContext(ctx))
			if !errors.Is(err, ErrDeferred) || requests != 1 {
				t.Fatalf("cooldown missed: requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestRetryAfterDoesNotCapLongWait(t *testing.T) {
	if got, ok := RetryAfter("600"); !ok || got != 10*time.Minute {
		t.Fatalf("delta=%s ok=%t", got, ok)
	}
	when := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	if got, ok := RetryAfter(when); !ok || got < 59*time.Minute {
		t.Fatalf("date=%s ok=%t", got, ok)
	}
}

func TestConcurrentPacingAndExtendedCooldown(t *testing.T) {
	u := testURL(t)
	var mu sync.Mutex
	var starts []time.Time
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		return response(http.StatusOK, nil), nil
	})
	tr := Wrap(base, 25*time.Millisecond)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tr.RoundTrip((&http.Request{Method: "GET", URL: u}).WithContext(context.Background()))
			if err != nil {
				t.Errorf("request: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(starts) != 3 {
		t.Fatalf("requests=%d", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 20*time.Millisecond {
			t.Fatalf("starts too close: %v", starts)
		}
	}

	s := state(u)
	s.mu.Lock()
	s.next = time.Now().Add(40 * time.Millisecond)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := tr.RoundTrip((&http.Request{Method: "GET", URL: u}).WithContext(ctx))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	s.mu.Lock()
	s.next = time.Now().Add(time.Minute)
	s.mu.Unlock()
	if err := <-done; !errors.Is(err, ErrDeferred) {
		t.Fatalf("waiter ignored later cooldown: %v", err)
	}
	if len(starts) != 3 {
		t.Fatalf("waiter reached transport: %d", len(starts))
	}
}

func TestResetPreservesMilliseconds(t *testing.T) {
	expected := time.Now().Add(time.Hour).Truncate(time.Second).Add(999 * time.Millisecond)
	got, ok := resetTime(http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(expected.UnixMilli(), 10)}})
	if !ok || !got.Equal(expected) {
		t.Fatalf("reset shortened: got=%s want=%s", got, expected)
	}
	if delay, ok := RetryAfter("999999999999999999999999"); !ok || delay < 24*time.Hour {
		t.Fatalf("overflowed Retry-After: %s %v", delay, ok)
	}
	if _, ok := RetryAfter("invalid"); ok {
		t.Fatal("invalid Retry-After accepted")
	}
}

func TestStricterWrapperPreservesIntervalFromLastRequest(t *testing.T) {
	u := testURL(t)
	base := roundTripFunc(func(*http.Request) (*http.Response, error) { return response(http.StatusOK, nil), nil })
	req := (&http.Request{Method: "GET", URL: u}).WithContext(context.Background())
	start := time.Now()
	resp, err := Wrap(base, time.Millisecond).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = Wrap(base, 60*time.Millisecond).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if time.Since(start) < 55*time.Millisecond {
		t.Fatal("new wrapper weakened its requested interval")
	}
}
