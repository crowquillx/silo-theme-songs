package pluginapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/engine"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
)

func TestFailedIntentIsRetryable(t *testing.T) {
	d := destinations.Destination{LibraryID: "lib", LocalPath: "/media/Movie"}
	records := []engine.Record{{LibraryID: d.LibraryID, LocalPath: d.LocalPath, SourceID: "tmdb:1", Status: engine.Failed}}
	if _, ok := existing(records, d, "tmdb:1"); ok {
		t.Fatal("failed intent hides source retry")
	}
	if blocked(records, d) {
		t.Fatal("failed intent blocks owner")
	}
}

func TestEmptyConfigureResetsServer(t *testing.T) {
	s := New(&pluginv1.PluginManifest{PluginId: "test.plugin"}, "general", false, func(Config) (Provider, error) { return &fakeProvider{}, nil })
	defer s.Close()
	cfg := Defaults()
	cfg.BaseURL = "http://127.0.0.1:1"
	cfg.APIKey = "fixture-secret"
	cfg.ProfileID = "primary"
	cfg.StateDir = t.TempDir()
	cfg.LibraryIDs = []string{"lib"}
	cfg.Policy = destinations.Policy{AllowedRoots: []string{t.TempDir()}}
	if err := s.ConfigureLocal(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Configure(context.Background(), &pluginv1.ConfigureRequest{}); err != nil {
		t.Fatal(err)
	}
	if s.engine != nil || s.client != nil || s.provider != nil || s.cfg.APIKey != "" {
		t.Fatal("empty settings left active configuration")
	}
	if report := s.Execute(context.Background(), "sync"); report.State != "not_configured" {
		t.Fatalf("reset report=%+v", report)
	}
}

type safetyFixture struct {
	server *Server
	http   *httptest.Server
	config Config
	dest   destinations.Destination
	stage  string
}

func newSafetyFixture(t *testing.T, p Provider, manual bool) safetyFixture {
	t.Helper()
	base := t.TempDir()
	owner := filepath.Join(base, "Movie")
	if err := os.Mkdir(owner, 0755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(owner, "movie.mkv")
	if err := os.WriteFile(video, []byte("video"), 0644); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(t.TempDir(), "audio.bin")
	if err := os.WriteFile(stage, []byte("validated audio fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/catalog/themes/capabilities":
			fmt.Fprint(w, `{"state":"available"}`)
		case "/api/v2/libraries":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "lib", "type": "movies", "enabled": true, "paths": []string{base}}}})
		case "/api/v2/catalog":
			fmt.Fprint(w, `{"items":[{"content_id":"movie:1","type":"movie","title":"Movie"}]}`)
		case "/api/v2/catalog/items/movie:1":
			fmt.Fprint(w, `{"content_id":"movie:1","type":"movie","title":"Movie"}`)
		case "/api/v2/catalog/items/movie:1/versions":
			fmt.Fprint(w, `{"items":[{"file_id":"file1"}]}`)
		case "/api/v2/admin/items/movie:1/files":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "file1", "library_id": "lib", "file_path": video, "observed_root_path": owner}}})
		case "/api/v2/admin/autoscan/settings":
			fmt.Fprint(w, `{"enabled":true}`)
		case "/api/v2/admin/autoscan/sources":
			fmt.Fprint(w, `{"items":[{"id":"source1","plugin_id":"test.plugin","capability_id":"themes","enabled":true,"delivery_mode":"poll","path_rewrites":[]}]}`)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	s := New(&pluginv1.PluginManifest{PluginId: "test.plugin"}, "general", false, func(Config) (Provider, error) { return p, nil })
	t.Cleanup(func() { _ = s.Close() })
	cfg := Defaults()
	cfg.BaseURL = h.URL
	cfg.APIKey = "fixture-secret"
	cfg.ProfileID = "primary"
	cfg.StateDir = t.TempDir()
	cfg.LibraryIDs = []string{"lib"}
	cfg.Policy = destinations.Policy{AllowedRoots: []string{base}}
	cfg.PreviewOnly = false
	cfg.ManualRefresh = manual
	dest := destinations.Destination{LibraryID: "lib", ItemID: "movie:1", OwnerID: "movie:1", LocalPath: owner, ServerPath: owner, Fingerprint: "fixture"}
	return safetyFixture{s, h, cfg, dest, stage}
}

func TestSourceSelectionFailureIsNotSuccess(t *testing.T) {
	p := &fakeProvider{selectErr: errors.New("source temporarily unavailable")}
	f := newSafetyFixture(t, p, true)
	if err := f.server.ConfigureLocal(context.Background(), f.config); err != nil {
		t.Fatal(err)
	}
	report := f.server.Execute(context.Background(), "sync")
	if report.State != "needs_attention" || report.Downloaded != 0 || p.calls.Load() != 0 {
		t.Fatalf("source failure reported as success: %+v", report)
	}
	if len(report.Rows) != 1 || report.Rows[0].Action != "source_unavailable" || !strings.Contains(report.Rows[0].Reason, "temporarily unavailable") {
		t.Fatalf("source row=%+v", report.Rows)
	}
}

func TestPendingRecoveryWaitsForScanAcknowledgement(t *testing.T) {
	ctx := context.Background()
	p := &fakeProvider{}
	f := newSafetyFixture(t, p, false)
	store, err := engine.Open(f.config.StateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Publish(ctx, f.dest, "fixture-1", "general-fixture-1-title.ogg", f.stage, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(f.config.StateDir, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	records := state["records"].([]any)
	records[0].(map[string]any)["status"] = "pending"
	records[0].(map[string]any)["journal_seq"] = float64(0)
	state["events"] = []any{}
	state["next_seq"] = float64(0)
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.server.ConfigureLocal(ctx, f.config); err != nil {
		t.Fatal(err)
	}
	report := f.server.Execute(ctx, "sync")
	if got := f.server.engine.Records()[0].Status; got != engine.Pending {
		t.Fatalf("recovered before source readiness: %s", got)
	}
	if report.Downloaded != 0 || p.calls.Load() != 0 {
		t.Fatalf("wrote before source readiness: %+v", report)
	}
	first, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes"})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: first.NextMarker})
	if err != nil {
		t.Fatal(err)
	}
	f.server.Execute(ctx, "sync")
	if got := f.server.engine.Records()[0].Status; got != engine.Published {
		t.Fatalf("pending intent did not recover after acknowledgement: %s", got)
	}
	batch, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: ready.NextMarker})
	if err != nil || len(batch.Changes) != 1 {
		t.Fatalf("recovered event missing: %+v %v", batch, err)
	}
}

func TestSourceResetNeedsExplicitReconcile(t *testing.T) {
	ctx := context.Background()
	p := &fakeProvider{candidates: []Candidate{}}
	f := newSafetyFixture(t, p, false)
	if err := f.server.ConfigureLocal(ctx, f.config); err != nil {
		t.Fatal(err)
	}
	baseline, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: baseline.NextMarker}); err != nil {
		t.Fatal(err)
	}
	f.server.Execute(ctx, "sync") // Establish an empty, ready source generation.
	libs, err := f.server.client.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := f.server.client.Inventory(ctx, libs[0])
	if err != nil {
		t.Fatal(err)
	}
	dests, err := f.config.Policy.Resolve(inv, inv.Items[0], nil)
	if err != nil || len(dests) != 1 {
		t.Fatalf("destination=%+v %v", dests, err)
	}
	dest := dests[0]
	r, err := f.server.engine.Publish(ctx, dest, "fixture-1", "general-fixture-1-title.ogg", f.stage, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.engine.MarkDiscovered(dest, r.Filename, r.OwnerID, strings.TrimSuffix(r.Filename, ".ogg")); err != nil {
		t.Fatal(err)
	}
	f.config.SourceGeneration = "2"
	if err := f.server.ConfigureLocal(ctx, f.config); err != nil {
		t.Fatal(err)
	}
	reset, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes"})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: reset.NextMarker})
	if err != nil {
		t.Fatal(err)
	}
	report := f.server.Execute(ctx, "sync")
	if report.State != "reconcile_required" || report.Downloaded != 0 {
		t.Fatalf("reset silently accepted: %+v", report)
	}
	if p.calls.Load() != 0 {
		t.Fatal("downloaded during source reset")
	}
	reconcile := f.server.Execute(ctx, "reconcile")
	if reconcile.State == "prerequisite_failed" {
		t.Fatalf("reconcile failed: %+v", reconcile)
	}
	page, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: ready.NextMarker})
	if err != nil || len(page.Changes) != 1 {
		t.Fatalf("reconcile event missing: %+v %v", page, err)
	}
	report = f.server.Execute(ctx, "sync")
	if report.State == "reconcile_required" {
		t.Fatalf("reconcile gate did not clear: %+v", report)
	}
}

func TestUnsafeCandidateIDUsesHashFilename(t *testing.T) {
	c := Candidate{ID: "https://example.org/?token=secret", Title: "A Title", Extension: "mp3"}
	name := filename("general", c)
	if strings.Contains(name, "token") || !strings.HasPrefix(name, "general-hash-") {
		t.Fatalf("unsafe ID leaked into filename: %s", name)
	}
}

func TestReviewRequiredIsOperatorDecision(t *testing.T) {
	p := &fakeProvider{selectErr: &provider.ReviewRequired{Reason: "ambiguous title", Candidates: []provider.ReviewCandidate{{ID: "abcdefghijk", Title: "Possible opening", URL: "https://www.youtube.com/watch?v=abcdefghijk", Channel: "Example", Score: 12}}}}
	f := newSafetyFixture(t, p, true)
	if err := f.server.ConfigureLocal(context.Background(), f.config); err != nil {
		t.Fatal(err)
	}
	report := f.server.Execute(context.Background(), "sync")
	if report.State != "needs_review" || report.Downloaded != 0 || p.calls.Load() != 0 {
		t.Fatalf("review became download/success: %+v", report)
	}
	if len(report.Rows) != 1 || report.Rows[0].Action != "needs_review" || len(report.Rows[0].Candidates) != 1 {
		t.Fatalf("review row=%+v", report.Rows)
	}
	if !strings.Contains(report.Rows[0].Reason, "provider.url_overrides") {
		t.Fatalf("missing override instruction: %+v", report.Rows[0])
	}
	html, err := f.server.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/"})
	if err != nil || !strings.Contains(string(html.Body), "https://www.youtube.com/watch?v=abcdefghijk") {
		t.Fatalf("review link absent: %v %s", err, html.Body)
	}
}

func TestGeneralKeepsDiscoveredThemeWhenSelectionChanges(t *testing.T) {
	ctx := context.Background()
	p := &fakeProvider{candidates: []Candidate{{ID: "fixture-2", Title: "Replacement", Extension: "ogg", Provenance: "fixture"}}}
	f := newSafetyFixture(t, p, true)
	p.staged = f.stage
	if err := f.server.ConfigureLocal(ctx, f.config); err != nil {
		t.Fatal(err)
	}
	libs, err := f.server.client.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := f.server.client.Inventory(ctx, libs[0])
	if err != nil {
		t.Fatal(err)
	}
	dests, err := f.config.Policy.Resolve(inv, inv.Items[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	d := dests[0]
	r, err := f.server.engine.Publish(ctx, d, "fixture-1", "general-id-fixture_h1-old.ogg", f.stage, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.engine.MarkDiscovered(d, r.Filename, r.OwnerID, strings.TrimSuffix(r.Filename, ".ogg")); err != nil {
		t.Fatal(err)
	}
	report := f.server.Execute(ctx, "sync")
	if report.Downloaded != 0 || p.calls.Load() != 0 || len(f.server.engine.Records()) != 1 {
		t.Fatalf("general theme replaced/appended: %+v", report)
	}
	if !hasAction(report.Rows, "selection_changed") {
		t.Fatalf("selection change hidden: %+v", report.Rows)
	}
}

func TestRejectedScanPathIsVisibleButNotIndexed(t *testing.T) {
	ctx := context.Background()
	f := newSafetyFixture(t, &fakeProvider{}, false)
	if err := f.server.ConfigureLocal(ctx, f.config); err != nil {
		t.Fatal(err)
	}
	first, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes"})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: first.NextMarker})
	if err != nil {
		t.Fatal(err)
	}
	d := f.dest
	d.ServerPath = "/outside/allowed/root"
	if _, err := f.server.engine.Publish(ctx, d, "fixture-1", "general-fixture-1-title.ogg", f.stage, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	batch, err := f.server.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: ready.NextMarker})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Changes) != 0 {
		t.Fatalf("rejected path indexed: %+v", batch.Changes)
	}
	response, err := f.server.Handle(ctx, &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status"})
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(response.Body, &report); err != nil {
		t.Fatal(err)
	}
	if report.RepairCount != 1 || len(report.RepairPaths) != 1 || report.RepairPaths[0] != d.ServerPath || report.State != "needs_attention" {
		t.Fatalf("repair queue hidden: %+v", report)
	}
	html, err := f.server.Handle(ctx, &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/"})
	if err != nil || !strings.Contains(string(html.Body), "rejected from indexing") || !strings.Contains(string(html.Body), d.ServerPath) {
		t.Fatalf("repair queue absent in HTML: %v %s", err, html.Body)
	}
}

func TestAnimeObsoleteSelectionNeedsOperatorReview(t *testing.T) {
	d := destinations.Destination{LibraryID: "lib", ItemID: "series:1", LocalPath: "/media/Series", ServerPath: "/media/Series", Season: -1}
	report := Report{State: "complete"}
	records := []engine.Record{{Destination: d, LibraryID: d.LibraryID, LocalPath: d.LocalPath, ServerPath: d.ServerPath, SourceID: "theme:old", Filename: "anime-old.ogg", Status: engine.Discovered}}
	reportObsolete(&report, records, []destinations.Destination{d}, []Candidate{{ID: "theme:new"}})
	if len(report.Rows) != 1 || report.Rows[0].Action != "operator_review" || report.Rows[0].Theme != "anime-old.ogg" {
		t.Fatalf("obsolete theme not reported: %+v", report.Rows)
	}
	report.Rows = nil
	reportObsolete(&report, records, []destinations.Destination{d}, []Candidate{{ID: "theme:old"}})
	if len(report.Rows) != 0 {
		t.Fatalf("selected managed theme marked obsolete: %+v", report.Rows)
	}
}

func hasAction(rows []Row, action string) bool {
	for _, row := range rows {
		if row.Action == action {
			return true
		}
	}
	return false
}
