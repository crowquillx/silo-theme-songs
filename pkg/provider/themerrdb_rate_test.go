package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestThemerrDBCooldownSurvivesNewClient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	first := &ThemerrDB{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := first.Lookup(context.Background(), Movie, "10"); err == nil {
		t.Fatal("first lookup accepted HTTP 429")
	}
	second := &ThemerrDB{BaseURL: srv.URL, Client: srv.Client()}
	started := time.Now()
	if _, err := second.Lookup(context.Background(), Movie, "11"); err == nil || !strings.Contains(err.Error(), "transient") {
		t.Fatalf("second lookup: %v", err)
	}
	if calls.Load() != 1 || time.Since(started) > time.Second {
		t.Fatalf("new client sent request during cooldown: calls=%d elapsed=%s", calls.Load(), time.Since(started))
	}
}
