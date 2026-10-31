package mapping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMappingCooldownAcrossStores(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	first := NewStore(srv.Client())
	if _, err := first.fetch(context.Background(), srv.URL+"/mapping"); err == nil || !strings.Contains(err.Error(), "retry later") {
		t.Fatalf("first fetch: %v", err)
	}
	second := NewStore(srv.Client())
	started := time.Now()
	if _, err := second.fetch(context.Background(), srv.URL+"/mapping"); err == nil || !strings.Contains(err.Error(), "retry later") {
		t.Fatalf("second fetch: %v", err)
	}
	if calls.Load() != 1 || time.Since(started) > time.Second {
		t.Fatalf("new store requested during cooldown: calls=%d elapsed=%s", calls.Load(), time.Since(started))
	}
}

func TestMappingRedirectHopIsPaced(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/from" {
			http.Redirect(w, r, "/to", http.StatusFound)
			return
		}
		w.Write([]byte("snapshot"))
	}))
	defer srv.Close()
	store := NewStore(srv.Client())
	started := time.Now()
	data, err := store.fetch(context.Background(), srv.URL+"/from")
	if err != nil || string(data) != "snapshot" || calls.Load() != 2 {
		t.Fatalf("redirect: data=%s calls=%d err=%v", data, calls.Load(), err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("redirect bypassed origin pacing: %s", elapsed)
	}
}
