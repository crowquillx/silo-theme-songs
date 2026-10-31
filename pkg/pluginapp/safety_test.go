package pluginapp

import (
	"context"
	"encoding/json"
	"errors"
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
	silo := newFakeSilo(t, "movies")
	s, cfg := newTestServer(t, silo, &fakeProvider{}, nil)
	if err := s.ConfigureLocal(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if s.general != nil || s.client != nil || s.cfg.APIKey != "" || s.configErr != errNoAPIKey {
		t.Fatal("empty settings left active configuration")
	}
	if report := s.Execute(context.Background(), "sync", nil); report.State != "not_configured" {
		t.Fatalf("reset report=%+v", report)
	}
}

// movieDestination resolves the only movie of a fake Silo.
func movieDestination(t *testing.T, s *Server, cfg Config) destinations.Destination {
	t.Helper()
	ctx := context.Background()
	libs, err := s.client.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := s.client.Inventory(ctx, libs[0])
	if err != nil {
		t.Fatal(err)
	}
	dests, err := cfg.policy(libs).Resolve(inv, inv.Items[0], nil)
	if err != nil || len(dests) != 1 {
		t.Fatalf("destination=%+v %v", dests, err)
	}
	return dests[0]
}

func TestSourceSelectionFailureIsNotSuccess(t *testing.T) {
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "Movie")
	p := &fakeProvider{selectErr: errors.New("source temporarily unavailable")}
	s, cfg := newTestServer(t, silo, p, nil)
	if err := s.ConfigureLocal(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	report := s.Execute(context.Background(), "sync", nil)
	if report.State != "needs_attention" || report.Downloaded != 0 || p.calls.Load() != 0 {
		t.Fatalf("source failure reported as success: %+v", report)
	}
	if len(report.Rows) != 1 || report.Rows[0].Action != "source_unavailable" || !strings.Contains(report.Rows[0].Reason, "temporarily unavailable") {
		t.Fatalf("source row=%+v", report.Rows)
	}
}

func TestPendingRecoveryRequestsScan(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "Movie")
	silo.scanDiscovers = false
	p := &fakeProvider{}
	s, cfg := newTestServer(t, silo, p, nil)
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	dest := movieDestination(t, s, cfg)
	r, err := s.general.engine.Publish(ctx, dest, "fixture-movie:1", "general-fixture-1-title.mp3", stagedAudio(t), func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A crash after writing the file leaves the record pending.
	statePath := filepath.Join(cfg.StateDir, "state.json")
	data, _ := os.ReadFile(statePath)
	var state map[string]any
	json.Unmarshal(data, &state)
	state["records"].([]any)[0].(map[string]any)["status"] = "pending"
	data, _ = json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	report := s.Execute(ctx, "sync", nil)
	if got := s.general.engine.Records()[0]; got.Status != engine.Published || got.ScanRequests != 1 {
		t.Fatalf("pending intent not recovered: %+v", got)
	}
	want := filepath.Join(dest.ServerPath, "theme-music", r.Filename)
	if scans := silo.scanned(); len(scans) != 1 || scans[0] != want || report.Downloaded != 0 || p.calls.Load() != 0 {
		t.Fatalf("recovery scans=%v report=%+v", scans, report)
	}
	// The record waits for Silo; a later check finds it.
	silo.mu.Lock()
	silo.visible["movie:1"] = true
	silo.mu.Unlock()
	rn := &run{ctx: ctx, api: s.client, report: &Report{}}
	if found, err := s.discovered(rn, s.general, s.general.engine.Records()[0]); err != nil || !found || s.general.engine.Records()[0].Status != engine.Discovered {
		t.Fatalf("discovery after scan: %v %v", found, err)
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
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "Movie")
	p := &fakeProvider{selectErr: &provider.ReviewRequired{Reason: "ambiguous title", Candidates: []provider.ReviewCandidate{{ID: "abcdefghijk", Title: "Possible opening", URL: "https://www.youtube.com/watch?v=abcdefghijk", Channel: "Example", Score: 12}}}}
	s, cfg := newTestServer(t, silo, p, nil)
	if err := s.ConfigureLocal(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	report := s.Execute(context.Background(), "sync", nil)
	if report.State != "needs_review" || report.Downloaded != 0 || p.calls.Load() != 0 {
		t.Fatalf("review became download/success: %+v", report)
	}
	if len(report.Rows) != 1 || report.Rows[0].Action != "needs_review" || len(report.Rows[0].Candidates) != 1 {
		t.Fatalf("review row=%+v", report.Rows)
	}
	if !strings.Contains(report.Rows[0].Reason, "provider.url_overrides") {
		t.Fatalf("missing override instruction: %+v", report.Rows[0])
	}
	html, err := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/"})
	if err != nil || !strings.Contains(string(html.Body), "https://www.youtube.com/watch?v=abcdefghijk") {
		t.Fatalf("review link absent: %v %s", err, html.Body)
	}
}

func TestGeneralKeepsDiscoveredThemeWhenSelectionChanges(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "Movie")
	p := &fakeProvider{candidates: []Candidate{{ID: "fixture-2", Title: "Replacement", Extension: "ogg", Provenance: "fixture"}}}
	s, cfg := newTestServer(t, silo, p, nil)
	p.staged = stagedAudio(t)
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	d := movieDestination(t, s, cfg)
	r, err := s.general.engine.Publish(ctx, d, "fixture-1", "general-id-fixture_h1-old.ogg", p.staged, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.general.engine.MarkDiscovered(d, r.Filename, r.OwnerID, strings.TrimSuffix(r.Filename, ".ogg")); err != nil {
		t.Fatal(err)
	}
	report := s.Execute(ctx, "sync", nil)
	if report.Downloaded != 0 || p.calls.Load() != 0 || len(s.general.engine.Records()) != 1 {
		t.Fatalf("general theme replaced/appended: %+v", report)
	}
	if !hasAction(report.Rows, "selection_changed") {
		t.Fatalf("selection change hidden: %+v", report.Rows)
	}
}

func TestReadOnlyLibraryIsRefusedWithFix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to read-only permissions")
	}
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "Movie")
	if err := os.Chmod(silo.root, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(silo.root, 0755) })
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	if err := s.ConfigureLocal(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	report := s.Execute(context.Background(), "sync", nil)
	if report.State != "needs_attention" || p.calls.Load() != 0 || len(report.Errors) != 2 || !strings.Contains(report.Errors[0], "cannot write") || !strings.Contains(report.Errors[1], "remove :ro") {
		t.Fatalf("read-only library: %+v", report)
	}
	// The run does not count as the day's full run, so the next check retries.
	if saved, err := readSchedule(cfg.StateDir); err != nil || !saved.LastFullRun.IsZero() {
		t.Fatalf("blocked run recorded: %+v %v", saved, err)
	}
	preview := s.Execute(context.Background(), "preview", nil)
	if len(preview.Notices) != 1 || !strings.Contains(preview.Notices[0], "cannot download") {
		t.Fatalf("read-only preview: %+v", preview)
	}
}

func TestLegacyAnimeStateIsCopiedOnce(t *testing.T) {
	ctx := context.Background()
	cache := t.TempDir()
	legacy := filepath.Join(cache, "plugin-state", legacyAnimePlugin)
	old, err := engine.Open(legacy, animeName)
	if err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(t.TempDir(), "Show", "Season 01")
	if err := os.MkdirAll(owner, 0755); err != nil {
		t.Fatal(err)
	}
	d := destinations.Destination{LibraryID: "lib", ItemID: "series:1", OwnerID: "season:1", Season: 1, LocalPath: owner, ServerPath: owner, Fingerprint: "proof"}
	if _, err := old.Publish(ctx, d, "animethemes-1-2", "animethemes-id-1-op.mp3", stagedAudio(t), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "anime-lists.xml"), []byte("<anime-list/>"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "anime")
	if _, err := migrateLegacyAnime(cache, dir); err == nil {
		t.Fatal("copied while the AnimeThemes plugin holds its state")
	}
	old.Close()
	if copied, err := migrateLegacyAnime(cache, dir); err != nil || !copied {
		t.Fatalf("copy: %v %v", copied, err)
	}
	store, err := engine.Open(dir, animeName)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if records := store.Records(); len(records) != 1 || records[0].Filename != "animethemes-id-1-op.mp3" {
		t.Fatalf("records=%+v", records)
	}
	if _, err := os.Stat(filepath.Join(dir, "anime-lists.xml")); err != nil {
		t.Fatal(err)
	}
	if copied, err := migrateLegacyAnime(cache, dir); err != nil || copied {
		t.Fatalf("second copy: %v %v", copied, err)
	}
	if copied, err := migrateLegacyAnime("", filepath.Join(t.TempDir(), "x")); err != nil || copied {
		t.Fatal("copied without a cache directory")
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
