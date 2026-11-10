package pluginapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/engine"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestPublicationRequestsScanAndConfirmsDiscovery(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:fixture", "Fixture")
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(ctx, "sync", nil)
	if r.Downloaded != 1 || r.State != "complete" {
		t.Fatalf("publication failed: %+v", r)
	}
	records := s.general.engine.Records()
	// The source candidate is Ogg, but publication must use the staged MP3
	// extension so the scanner and later discovery agree on the actual file.
	if len(records) != 1 || !strings.HasSuffix(records[0].Filename, ".mp3") || records[0].Status != engine.Discovered {
		t.Fatalf("theme not published and discovered: %+v", records)
	}
	want := filepath.Join(silo.root, "Fixture", "theme-music", records[0].Filename)
	if scans := silo.scanned(); !slices.Equal(scans, []string{want}) {
		t.Fatalf("scan requests %v, want %s", scans, want)
	}
	r = s.Execute(ctx, "sync", nil)
	if r.Downloaded != 0 || p.calls.Load() != 1 || len(silo.scanned()) != 1 {
		t.Fatalf("repeat downloaded or rescanned: %+v", r)
	}
	if !hasAction(r.Rows, "preserved") && !hasAction(r.Rows, "discovered") {
		t.Fatalf("repeat run rows: %+v", r.Rows)
	}
	status, e := s.Handle(ctx, &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status"})
	if e != nil || strings.Contains(string(status.Body), "fixture-secret") {
		t.Fatal("credential in report")
	}
}

func TestThemeWaitsForLibraryScanWithoutScanAPI(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:fixture", "Fixture")
	silo.scanStatus = http.StatusNotFound
	s, cfg := newTestServer(t, silo, &fakeProvider{staged: stagedAudio(t)}, nil)
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(ctx, "sync", nil)
	if r.Downloaded != 1 || len(r.Notices) != 1 || !strings.Contains(r.Notices[0], "next library scan") {
		t.Fatalf("missing scan API not explained: %+v", r)
	}
	if records := s.general.engine.Records(); len(records) != 1 || records[0].Status != engine.Published || records[0].ScanRequests != 1 {
		t.Fatalf("record=%+v", records)
	}
	if row := r.Rows[0]; row.Action != "downloaded" || !strings.Contains(row.Reason, "after it scans") {
		t.Fatalf("row=%+v", row)
	}
}

func TestDryRunListsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:fixture", "Fixture")
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	yes := true
	cfg.DryRun = &yes
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(ctx, "sync", nil)
	if r.Downloaded != 0 || p.calls.Load() != 0 || len(r.Rows) != 1 || r.Rows[0].Action != "would_download" || len(silo.scanned()) != 0 {
		t.Fatalf("dry run wrote: %+v", r)
	}
}

func TestTaskKeysAndRoutes(t *testing.T) {
	if taskMode("plugin:12:reconcile") != "sync" || taskMode("plugin:12:preview") != "preview" || taskMode("delete") != "" {
		t.Fatal("task routing")
	}
	s := New(&pluginv1.PluginManifest{}, nil, nil)
	for _, c := range []struct {
		method, path string
		body         string
		status       int32
	}{{"POST", "/", "", 405}, {"GET", "/run", "", 405}, {"GET", "/other", "", 404}, {"POST", "/run", "mode=delete", 400}, {"POST", "/run", "mode=sync", 303}} {
		r, e := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: c.method, Path: c.path, Body: []byte(c.body)})
		if e != nil || r.StatusCode != c.status {
			t.Errorf("%s %s = %d, %v; want %d", c.method, c.path, r.GetStatusCode(), e, c.status)
		}
	}
}

func TestRunOutlivesHostDeadline(t *testing.T) {
	release := make(chan struct{})
	var capabilityRequests atomic.Int32
	silo := newFakeSilo(t, "movies")
	gate := silo.Config.Handler
	silo.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/catalog/themes/capabilities" {
			capabilityRequests.Add(1)
			<-release
		}
		gate.ServeHTTP(w, r)
	})
	s, cfg := newTestServer(t, silo, &fakeProvider{}, nil)
	yes := true
	cfg.DryRun = &yes
	if e := s.ConfigureLocal(context.Background(), cfg); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	r, e := s.Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "plugin:1:preview"})
	if e != nil || ctx.Err() != nil {
		t.Fatalf("Run did not answer before its deadline: %v", e)
	}
	if got := r.GetOutput().AsMap()["state"]; got != "running" {
		t.Fatalf("state %v, want running", got)
	}
	// A second trigger joins the running task instead of queueing another,
	// and with dry run on, Download joins Preview.
	for _, key := range []string{"plugin:1:preview", "plugin:1:sync"} {
		again, againCancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
		if _, e := s.Run(again, &pluginv1.RunScheduledTaskRequest{TaskKey: key}); e != nil {
			t.Fatal(e)
		}
		againCancel()
	}
	running, e := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status"})
	if e != nil || !strings.Contains(string(running.Body), `"progress":"Preview: starting"`) || !strings.Contains(string(running.Body), `"state":"configured"`) {
		t.Fatalf("running status lacks progress or the last report: %s", running.GetBody())
	}
	if n := capabilityRequests.Load(); n != 1 {
		t.Fatalf("%d concurrent preview runs, want one", n)
	}
	close(release)
	r, e = s.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "plugin:1:preview"})
	if e != nil {
		t.Fatal(e)
	}
	if got := r.GetOutput().AsMap()["state"]; got != "complete" {
		t.Fatalf("background result %v, want complete", r.GetOutput().AsMap())
	}
	status, e := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status"})
	if e != nil || !strings.Contains(string(status.Body), `"state":"complete"`) {
		t.Fatalf("status page lacks the background result: %s", status.GetBody())
	}
}

func TestSettingsDefaults(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("SILO_PLUGIN_CACHE_DIR", cache)
	c := Defaults()
	c.APIKey = "key"
	c = c.withDefaults(context.Background(), "test.plugin")
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	if c.siloURL != fallbackSiloURL || c.StateDir != filepath.Join(cache, "plugin-state", "test.plugin") || c.cacheDir != cache {
		t.Fatalf("defaults: %q %q %q", c.siloURL, c.StateDir, c.cacheDir)
	}
	if c.dryRun() || !c.animeEnabled() || c.MaxDownloads != 0 {
		t.Fatal("a fresh install does not download everything")
	}
	libs := []siloapi.Library{{ID: "1", Type: "movies", Paths: []string{"/media/movies"}}, {ID: "2", Type: "series", Paths: []string{"/media/tv"}}, {ID: "3", Type: "music", Paths: []string{"/media/music"}}}
	if p := c.policy(libs); !slices.Equal(p.AllowedRoots, []string{"/media/movies", "/media/tv"}) {
		t.Fatalf("default roots %v", p.AllowedRoots)
	}
	c.Policy.Mounts = []destinations.Mount{{Server: "/media", Local: "/mnt/media"}}
	if p := c.policy(libs); !slices.Equal(p.AllowedRoots, []string{"/mnt/media/movies", "/mnt/media/tv"}) {
		t.Fatalf("mapped default roots %v", p.AllowedRoots)
	}
	c.Policy.AllowedRoots = []string{"/mnt/media/movies"}
	if p := c.policy(libs); !slices.Equal(p.AllowedRoots, c.Policy.AllowedRoots) {
		t.Fatal("explicit roots replaced")
	}
	t.Setenv("SILO_PLUGIN_CACHE_DIR", "")
	bare := Defaults()
	bare.APIKey = "key"
	if e := bare.withDefaults(context.Background(), "test.plugin").Validate(); e == nil || !strings.Contains(e.Error(), "Persistent state directory") {
		t.Fatalf("missing state_dir accepted: %v", e)
	}
	if e := Defaults().Validate(); e != errNoAPIKey {
		t.Fatalf("missing key: %v", e)
	}
}

func TestLibrarySelectionAndLegacyPreview(t *testing.T) {
	movies, shows, music := siloapi.Library{ID: "1", Type: "movies"}, siloapi.Library{ID: "2", Type: "series"}, siloapi.Library{ID: "3", Type: "music"}
	c := Defaults()
	if !c.selected(movies) || !c.selected(shows) || c.selected(music) {
		t.Fatal("empty selection does not mean every movie and series library")
	}
	c.SkipLibraryIDs = []string{"2"}
	if !c.selected(movies) || c.selected(shows) {
		t.Fatal("skipped library used")
	}
	c.SkipLibraryIDs, c.LibraryIDs = nil, []string{"2", "3"}
	if c.selected(movies) || !c.selected(shows) || c.selected(music) {
		t.Fatal("only-these list ignored")
	}
	if !c.animeLibrary("1") {
		t.Fatal("empty anime list does not cover every library")
	}
	c.AnimeLibraryIDs = []string{"2"}
	if c.animeLibrary("1") || !c.animeLibrary("2") {
		t.Fatal("anime library list ignored")
	}
	yes, no := true, false
	c.PreviewOnly = &yes
	if !c.dryRun() {
		t.Fatal("saved preview_only from 0.1 ignored")
	}
	c.DryRun = &no
	if c.dryRun() {
		t.Fatal("dry_run does not replace preview_only")
	}
}

func TestInstalledCacheDir(t *testing.T) {
	for exe, want := range map[string]string{
		"/var/lib/silo/plugins/test.plugin/0.1.14/install-2598497015/plugin": "/var/lib/silo/plugins",
		"/var/lib/silo/plugins/other.plugin/0.1.14/install-1/plugin":         "",
		"/var/lib/silo/plugins/test.plugin/0.1.14/tmp/plugin":                "",
		"/usr/local/bin/plugin": "",
		"plugin":                "",
	} {
		if got := installedCacheDir(exe, "test.plugin"); got != want {
			t.Errorf("installedCacheDir(%q) = %q, want %q", exe, got, want)
		}
	}
	if got := installedCacheDir("/cache/test.plugin/1/install-1/plugin", ""); got != "" {
		t.Errorf("empty plugin ID gave %q", got)
	}
}

func TestNoSelectedLibraries(t *testing.T) {
	silo := newFakeSilo(t, "movies")
	s, cfg := newTestServer(t, silo, &fakeProvider{}, nil)
	cfg.SkipLibraryIDs = []string{"lib"}
	if e := s.ConfigureLocal(context.Background(), cfg); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(context.Background(), "preview", nil)
	if r.State != "prerequisite_failed" || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "No movie or series libraries") {
		t.Fatalf("no libraries: %+v", r)
	}
}

func TestReportKeepsProblemsVisibleAndCountsEverything(t *testing.T) {
	var r Report
	for i := range 1500 {
		r.add(Row{ItemID: fmt.Sprint(i), Action: "preserved", Reason: "existing root theme is preserved"})
	}
	r.add(Row{ItemID: "late", Action: "download_failed", Reason: "YouTube video is unavailable"})
	r.add(Row{ItemID: "planned", Action: "would_download"})
	r.finish()
	if len(r.Rows) != displayRows || !r.Truncated || r.Rows[0].ItemID != "late" || r.Rows[1].ItemID != "planned" {
		t.Fatalf("display rows: %d truncated=%v first=%+v", len(r.Rows), r.Truncated, r.Rows[:2])
	}
	if len(r.Summary) != 3 || r.Summary[0] != (SummaryRow{Action: "preserved", Reason: "existing root theme is preserved", Count: 1500}) {
		t.Fatalf("summary %+v", r.Summary)
	}
}

func TestRunsLogJSONWithoutSecrets(t *testing.T) {
	silo := newFakeSilo(t, "movies")
	s, cfg := newTestServer(t, silo, &fakeProvider{}, nil)
	var logs strings.Builder
	s.log = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true, Level: hclog.Info})
	if e := s.ConfigureLocal(context.Background(), cfg); e != nil {
		t.Fatal(e)
	}
	s.Execute(context.Background(), "sync", nil)
	s.Execute(context.Background(), "preview", nil)
	var finished int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if e := json.Unmarshal([]byte(line), &entry); e != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if entry["@message"] == "task finished" && entry["@level"] == "info" {
			finished++
		}
	}
	if finished != 2 || strings.Contains(logs.String(), "fixture-secret") {
		t.Fatalf("logs: %s", logs.String())
	}
}

// Silo's settings form saves JSON settings as text, sends an emptied number
// as null, and keeps settings from earlier versions.
func TestSettingsFromFormText(t *testing.T) {
	settings := map[string]any{
		"api_key":           "key",
		"library_ids":       ` ["3", "4"] `,
		"skip_library_ids":  "8",
		"exclude_items":     "5, 6 7",
		"anime_library_ids": "",
		"max_downloads":     nil,
		"destinations":      `{"path_mappings":[{"server":"/media","local":"/mnt/media"}]}`,
		"provider":          `{"manage_tools":false}`,
		"anime_provider":    `{"op":false}`,
		"preview_only":      true,
		"manual_refresh":    true,
		"source_generation": "1",
	}
	c, e := parseSettings(settings)
	if e != nil {
		t.Fatal(e)
	}
	if !slices.Equal(c.LibraryIDs, []string{"3", "4"}) || !slices.Equal(c.SkipLibraryIDs, []string{"8"}) || !slices.Equal(c.ExcludeItems, []string{"5", "6", "7"}) || c.AnimeLibraryIDs != nil {
		t.Fatalf("lists: %q %q %q %q", c.LibraryIDs, c.SkipLibraryIDs, c.ExcludeItems, c.AnimeLibraryIDs)
	}
	if len(c.Policy.Mounts) != 1 || c.Policy.Mounts[0].Local != "/mnt/media" || string(c.Provider) != `{"manage_tools":false}` || string(c.AnimeProvider) != `{"op":false}` {
		t.Fatalf("objects: %+v %s %s", c.Policy, c.Provider, c.AnimeProvider)
	}
	if !c.dryRun() || c.MaxDownloads != 0 {
		t.Fatalf("legacy or cleared settings: %+v", c)
	}
	if settings["library_ids"] != ` ["3", "4"] ` {
		t.Fatal("parsing changed the stored settings")
	}
	if _, e := parseSettings(map[string]any{"destinations": `{"allowed_roots":`}); e == nil || !strings.Contains(e.Error(), "destinations") {
		t.Fatalf("bad JSON accepted: %v", e)
	}
	if _, e := parseSettings(map[string]any{"api_key": "key", "unknown": true}); e == nil {
		t.Fatal("unknown setting accepted")
	}
}

func TestConfigureKeepsPluginUsableWithoutSettings(t *testing.T) {
	s := New(&pluginv1.PluginManifest{PluginId: "test.plugin"}, func(Config) (Provider, error) { return &fakeProvider{}, nil }, nil)
	defer s.Close()
	value, _ := structpb.NewStruct(map[string]any{"library_ids": "3"})
	if e := s.configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); e != nil {
		t.Fatalf("configure failed instead of reporting: %v", e)
	}
	if s.general != nil || s.configErr != errNoAPIKey {
		t.Fatalf("missing key: %v", s.configErr)
	}
	html, e := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/"})
	if e != nil || !strings.Contains(string(html.Body), "Setup needed") || !strings.Contains(string(html.Body), `href="/admin/api-keys"`) {
		t.Fatalf("setup page: %v %s", e, html.GetBody())
	}
	if r := s.Execute(context.Background(), "sync", nil); r.State != "not_configured" || !strings.Contains(r.Errors[0], "API key") {
		t.Fatalf("unconfigured run: %+v", r)
	}
	value, _ = structpb.NewStruct(map[string]any{"api_key": "key", "destinations": "{"})
	if e := s.configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); e != nil || s.configErr == nil || !strings.Contains(s.configErr.Error(), "destinations") {
		t.Fatalf("bad setting: %v %v", e, s.configErr)
	}
}

func TestStatusPageListsLibraries(t *testing.T) {
	silo := newFakeSilo(t, "movies")
	s, cfg := newTestServer(t, silo, &fakeProvider{}, nil)
	if e := s.ConfigureLocal(context.Background(), cfg); e != nil {
		t.Fatal(e)
	}
	html, e := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/"})
	body := string(html.GetBody())
	if e != nil || !strings.Contains(body, `name="library" value="lib" checked`) || strings.Contains(body, "Music") || !strings.Contains(body, `action="run"`) {
		t.Fatalf("library list: %v %s", e, body)
	}
	if csp := html.Headers["Content-Security-Policy"]; !strings.Contains(csp, "form-action 'self'") {
		t.Fatalf("CSP %q", csp)
	}
}

func TestAnimeSeriesUseAnimeThemesAndOthersThemerrDB(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "series")
	silo.addSeries("series:anime", "Anime Show", "1")
	silo.addSeries("series:western", "Western Show", "2")
	stage := stagedAudio(t)
	general := &fakeProvider{staged: stage}
	anime := &fakeProvider{staged: stage, claims: map[string]bool{"series:anime": true}}
	s, cfg := newTestServer(t, silo, general, anime)
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(ctx, "sync", nil)
	if r.Downloaded != 2 || general.calls.Load() != 1 || anime.calls.Load() != 1 {
		t.Fatalf("routing: %+v", r)
	}
	animeRecords, generalRecords := s.anime.engine.Records(), s.general.engine.Records()
	if len(animeRecords) != 1 || animeRecords[0].Destination.ItemID != "series:anime" || animeRecords[0].Destination.Season != 1 || !strings.HasPrefix(animeRecords[0].Filename, "animethemes-") {
		t.Fatalf("anime records: %+v", animeRecords)
	}
	if len(generalRecords) != 1 || generalRecords[0].Destination.ItemID != "series:western" || generalRecords[0].Destination.Season != -1 || !strings.HasPrefix(generalRecords[0].Filename, "general-") {
		t.Fatalf("general records: %+v", generalRecords)
	}
	if !strings.HasPrefix(animeRecords[0].LocalPath, filepath.Join(silo.root, "Anime Show", "Season 01")) || animeRecords[0].Status != engine.Discovered {
		t.Fatalf("anime destination: %+v", animeRecords[0])
	}
	// An item keeps the source that manages it, even if AnimeThemes would
	// now claim it.
	anime.claims["series:western"] = true
	r = s.Execute(ctx, "sync", nil)
	if r.Downloaded != 0 || anime.calls.Load() != 1 || general.calls.Load() != 1 {
		t.Fatalf("source changed for a managed item: %+v", r)
	}
	// With AnimeThemes off, ThemerrDB handles every series. Season themes
	// AnimeThemes downloaded stay.
	silo.addSeries("series:new", "New Show", "3")
	anime.claims["series:new"] = true
	no := false
	cfg.AnimeThemes = &no
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r = s.Execute(ctx, "sync", nil)
	if r.Downloaded != 2 || general.calls.Load() != 3 || anime.calls.Load() != 1 || len(s.anime.engine.Records()) != 1 {
		t.Fatalf("AnimeThemes still used when off: %+v", r)
	}
}

func TestSeparateAnimeThemesPluginKeepsAnime(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "series")
	silo.addSeries("series:anime", "Anime Show", "1")
	silo.addSeries("series:western", "Western Show", "2")
	stage := stagedAudio(t)
	general := &fakeProvider{staged: stage}
	anime := &fakeProvider{staged: stage, claims: map[string]bool{"series:anime": true}}
	s, cfg := newTestServer(t, silo, general, anime)
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if r := s.Execute(ctx, "sync", nil); r.Downloaded != 2 {
		t.Fatalf("first run: %+v", r)
	}
	// Restart with the separate plugin installed: the run leaves the anime
	// series alone, opening AnimeThemes' state only to see what it manages.
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	s.legacyAnime = func(context.Context) bool { return true }
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	silo.addSeries("series:new", "New Show", "3")
	r := s.Execute(ctx, "sync", nil)
	if len(r.Notices) != 1 || !strings.Contains(r.Notices[0], "Uninstall the separate AnimeThemes plugin") {
		t.Fatalf("notice: %+v", r.Notices)
	}
	if r.Downloaded != 1 || general.calls.Load() != 2 || anime.calls.Load() != 1 || hasAction(r.Rows, "would_download") {
		t.Fatalf("deferred run: %+v", r)
	}
	for _, row := range r.Rows {
		if row.ItemID == "series:anime" {
			t.Fatalf("anime series touched: %+v", row)
		}
	}
	// Libraries listed for AnimeThemes are left to the separate plugin.
	cfg.AnimeLibraryIDs = []string{"lib"}
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if r := s.Execute(ctx, "preview", nil); len(r.Rows) != 1 || r.Rows[0].Action != "excluded" {
		t.Fatalf("anime library: %+v", r.Rows)
	}
}

func TestNewItemRunAfterLibraryScan(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "First")
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	s.auto, s.startDelay, s.scanDelay = true, time.Hour, 10*time.Millisecond
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if r := s.Execute(ctx, "sync", nil); r.Downloaded != 1 {
		t.Fatalf("full run: %+v", r)
	}
	saved, err := readSchedule(cfg.StateDir)
	if err != nil || saved.LastFullRun.IsZero() || !slices.Equal(saved.Known["lib"], []string{"movie:1"}) {
		t.Fatalf("schedule after full run: %+v %v", saved, err)
	}
	silo.addMovie("movie:2", "Second")
	payload, _ := structpb.NewStruct(map[string]any{"raw": "lib"})
	if _, e := s.HandleEvent(ctx, &pluginv1.HandleEventRequest{EventName: "scan_complete", Payload: payload}); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		r := s.newReport
		s.mu.RUnlock()
		if !r.At.IsZero() {
			if !r.NewItems || r.Downloaded != 1 || len(r.Rows) != 1 || r.Rows[0].ItemID != "movie:2" {
				t.Fatalf("new item run: %+v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no run after the library scan")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if saved, _ = readSchedule(cfg.StateDir); !slices.Equal(saved.Known["lib"], []string{"movie:1", "movie:2"}) {
		t.Fatalf("known items %v", saved.Known["lib"])
	}
	// The scan the plugin requested for movie 2 finds nothing new.
	calls := silo.apiCalls.Load()
	s.runNewItems(ctx)
	if s.HandleEvent(ctx, &pluginv1.HandleEventRequest{EventName: "other"}); p.calls.Load() != 2 {
		t.Fatal("extra download")
	}
	if silo.apiCalls.Load()-calls > 3 {
		t.Fatalf("%d API calls to find no new items", silo.apiCalls.Load()-calls)
	}
}

func TestAutomaticFullRunWhenDue(t *testing.T) {
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "First")
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	s.auto, s.startDelay = true, 10*time.Millisecond
	if e := s.ConfigureLocal(context.Background(), cfg); e != nil {
		t.Fatal(e)
	}
	// Silo's first request turns automatic runs on.
	if _, e := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status"}); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no automatic run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		saved, _ := readSchedule(cfg.StateDir)
		if saved.Settings == cfg.withDefaults(context.Background(), "test.plugin").fingerprint() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic run not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// autoLoops reports whether automatic runs are on and how many automate loops
// have started.
func autoLoops(s *Server) (bool, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active, s.loops
}

// Check Connection configures a temporary plugin process with unsaved
// settings, checks them and stops the process. It must not start a run.
func TestConfigureAloneStartsNoRun(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "First")
	p := &fakeProvider{staged: stagedAudio(t)}
	s, _ := newTestServer(t, silo, p, nil)
	s.auto, s.startDelay = true, 10*time.Millisecond
	value, _ := structpb.NewStruct(map[string]any{"api_key": "fixture-secret", "profile_id": "primary", "base_url": silo.URL, "state_dir": t.TempDir()})
	entries := []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}
	if e := s.configure(ctx, entries); e != nil || s.general == nil {
		t.Fatalf("configure: %v %v", e, s.configErr)
	}
	if r, e := s.TestConfiguration(ctx, &pluginv1.TestConfigurationRequest{Config: entries}); e != nil || !r.Ok {
		t.Fatalf("settings check: %+v %v", r, e)
	}
	// Configuring again, as a later settings change would, starts nothing either.
	if e := s.configure(ctx, entries); e != nil {
		t.Fatal(e)
	}
	time.Sleep(300 * time.Millisecond)
	active, loops := autoLoops(s)
	s.mu.RLock()
	jobs, mode := len(s.jobs), s.report.Mode
	s.mu.RUnlock()
	if active || loops != 0 || jobs != 0 || mode != "" || p.calls.Load() != 0 {
		t.Fatalf("configure started automatic runs: active=%v loops=%d jobs=%d last run %q downloads=%d", active, loops, jobs, mode, p.calls.Load())
	}
}

func TestFirstRequestStartsAutomaticRunsOnce(t *testing.T) {
	ctx := context.Background()
	requests := map[string]func(*Server) error{
		"task": func(s *Server) error {
			_, e := s.Run(ctx, &pluginv1.RunScheduledTaskRequest{TaskKey: "plugin:1:preview"})
			return e
		},
		"event": func(s *Server) error {
			_, e := s.HandleEvent(ctx, &pluginv1.HandleEventRequest{EventName: "other"})
			return e
		},
		"page": func(s *Server) error {
			_, e := s.Handle(ctx, &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status"})
			return e
		},
	}
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			silo := newFakeSilo(t, "movies")
			s, cfg := newTestServer(t, silo, &fakeProvider{}, nil)
			s.auto, s.startDelay = true, time.Hour
			if e := s.ConfigureLocal(ctx, cfg); e != nil {
				t.Fatal(e)
			}
			if active, loops := autoLoops(s); active || loops != 0 {
				t.Fatalf("before the request: active=%v loops=%d", active, loops)
			}
			if e := request(s); e != nil {
				t.Fatal(e)
			}
			if active, loops := autoLoops(s); !active || loops != 1 {
				t.Fatalf("after the request: active=%v loops=%d", active, loops)
			}
			// Later requests of every kind leave the one loop running.
			for _, again := range requests {
				if e := again(s); e != nil {
					t.Fatal(e)
				}
			}
			if _, loops := autoLoops(s); loops != 1 {
				t.Fatalf("%d loops after more requests", loops)
			}
		})
	}
}

// A request before the settings arrive starts the loop once they do, and new
// settings in a running plugin replace the loop and get their own full run.
func TestAutomaticRunsFollowNewSettings(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	silo.addMovie("movie:1", "First")
	s, cfg := newTestServer(t, silo, &fakeProvider{staged: stagedAudio(t)}, nil)
	s.auto, s.startDelay = true, 10*time.Millisecond
	if _, e := s.Handle(ctx, &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status"}); e != nil {
		t.Fatal(e)
	}
	if active, loops := autoLoops(s); !active || loops != 0 {
		t.Fatalf("unconfigured: active=%v loops=%d", active, loops)
	}
	recorded := func(cfg Config) {
		t.Helper()
		want := cfg.withDefaults(ctx, "test.plugin").fingerprint()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if saved, _ := readSchedule(cfg.StateDir); saved.Settings == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("no automatic run with the new settings")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	recorded(cfg)
	cfg.MaxDownloads = 5
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	recorded(cfg)
	if _, loops := autoLoops(s); loops != 2 {
		t.Fatalf("%d loops after two configurations", loops)
	}
	// Settings the plugin cannot use stop the loop.
	if e := s.configure(ctx, nil); e != nil {
		t.Fatal(e)
	}
	s.mu.RLock()
	loops, running := s.loops, s.stopAuto != nil
	s.mu.RUnlock()
	if loops != 2 || running {
		t.Fatalf("loops=%d running=%v after clearing the settings", loops, running)
	}
}

func TestStatusPageFollowsHostTheme(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	s, cfg := newTestServer(t, silo, &fakeProvider{}, nil)
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	themed := func(theme string) *structpb.Struct {
		q, _ := structpb.NewStruct(map[string]any{"theme": theme})
		return q
	}
	get := func(q *structpb.Struct) string {
		t.Helper()
		r, e := s.Handle(ctx, &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/", Query: q})
		if e != nil || r.StatusCode != 200 {
			t.Fatalf("page: %v %d", e, r.GetStatusCode())
		}
		if csp := r.Headers["Content-Security-Policy"]; csp != "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'self'" {
			t.Fatalf("CSP %q", csp)
		}
		return string(r.Body)
	}
	// Without a theme the browser's preference decides.
	body := get(nil)
	if !strings.Contains(body, `<html lang="en">`) || !strings.Contains(body, "@media (prefers-color-scheme:dark){:root:not([data-theme]){color-scheme:dark;") || !strings.Contains(body, ":root[data-theme=dark]{color-scheme:dark;") {
		t.Fatalf("page without a theme: %.900s", body)
	}
	if strings.Contains(body, "?theme=") || strings.Contains(body, "<script") {
		t.Fatal("page without a theme carries one, or has a script")
	}
	for theme, scheme := range map[string]string{"midnight-cinema": "dark", "oxblood-noir": "dark", "cinema-light": "light"} {
		body := get(themed(theme))
		for _, want := range []string{`<html lang="en" data-theme="` + scheme + `">`, `action="run?theme=` + theme + `"`, `action="libraries?theme=` + theme + `"`} {
			if !strings.Contains(body, want) {
				t.Errorf("theme %s: page lacks %s", theme, want)
			}
		}
	}
	// Unknown values select nothing and never reach the page.
	for _, theme := range []string{"neon", "dark", `"><script>alert(1)</script>`, "midnight-cinema;x", "MIDNIGHT-CINEMA"} {
		body := get(themed(theme))
		if !strings.Contains(body, `<html lang="en">`) || strings.Contains(body, "?theme=") || strings.Contains(body, "neon") || strings.Contains(body, "alert") || strings.Contains(body, "MIDNIGHT") {
			t.Errorf("unknown theme %q reached the page", theme)
		}
	}
	// Silo may send a repeated parameter as a list.
	list, _ := structpb.NewStruct(map[string]any{"theme": []any{"midnight-cinema", "cinema-light"}})
	if body := get(list); !strings.Contains(body, `data-theme="dark"`) {
		t.Error("theme list ignored")
	}
}

func TestRedirectsKeepHostTheme(t *testing.T) {
	ctx := context.Background()
	s := New(&pluginv1.PluginManifest{}, nil, nil)
	defer s.Close()
	themed := func(theme string) *structpb.Struct {
		q, _ := structpb.NewStruct(map[string]any{"theme": theme})
		return q
	}
	for _, c := range []struct {
		query *structpb.Struct
		want  string
	}{{nil, "./"}, {themed("midnight-cinema"), "./?theme=midnight-cinema"}, {themed("cinema-light"), "./?theme=cinema-light"}, {themed("neon"), "./"}, {themed("x\r\nSet-Cookie: a=b"), "./"}, {themed("midnight-cinema&saved=libraries"), "./"}} {
		r, e := s.Handle(ctx, &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/run", Body: []byte("mode=sync"), Query: c.query})
		if e != nil || r.StatusCode != 303 || r.Headers["Location"] != c.want {
			t.Errorf("run with %v: %d %q %v; want %q", c.query, r.GetStatusCode(), r.GetHeaders()["Location"], e, c.want)
		}
	}
	if got := pageURL("midnight-cinema", "libraries"); got != "./?saved=libraries&theme=midnight-cinema" {
		t.Errorf("libraries redirect %q", got)
	}
	if got := pageURL("", "libraries"); got != "./?saved=libraries" {
		t.Errorf("libraries redirect without a theme %q", got)
	}
}

func TestEventLibraryPayloads(t *testing.T) {
	for raw, want := range map[string]string{`{"raw":"12"}`: "12", `{"library_id":7}`: "7", `{}`: ""} {
		var m map[string]any
		json.Unmarshal([]byte(raw), &m)
		p, _ := structpb.NewStruct(m)
		if got := eventLibrary(p); got != want {
			t.Errorf("eventLibrary(%s) = %q, want %q", raw, got, want)
		}
	}
	if eventLibrary(nil) != "" {
		t.Error("nil payload")
	}
}

func TestSettingsCheck(t *testing.T) {
	silo := newFakeSilo(t, "movies")
	s, _ := newTestServer(t, silo, &fakeProvider{}, nil)
	check := func(settings map[string]any) *pluginv1.TestConfigurationResponse {
		value, _ := structpb.NewStruct(settings)
		r, e := s.TestConfiguration(context.Background(), &pluginv1.TestConfigurationRequest{Config: []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	state := t.TempDir()
	if r := check(map[string]any{"state_dir": state}); r.Ok || !strings.Contains(r.Message, "API key") {
		t.Fatalf("no key: %+v", r)
	}
	if r := check(map[string]any{"api_key": "fixture-secret", "profile_id": "primary", "base_url": silo.URL, "state_dir": state}); !r.Ok {
		t.Fatalf("good settings: %+v", r)
	}
	if r := check(map[string]any{"api_key": "fixture-secret", "profile_id": "primary", "base_url": silo.URL, "state_dir": state, "library_ids": "9"}); r.Ok || !strings.Contains(r.Message, "Library 9") {
		t.Fatalf("missing library: %+v", r)
	}
	if s.general != nil {
		t.Fatal("check changed the running configuration")
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

func TestSeasonTargetsCarryFilePaths(t *testing.T) {
	item := siloapi.Item{ID: "show", Type: "series"}
	inv := siloapi.Inventory{
		Seasons: map[string][]siloapi.Season{"show": {{Number: 1}, {Number: 2}}},
		Files: []siloapi.Membership{
			{File: siloapi.File{Path: "/vfs/Show/Season 01/a.mkv"}, ItemID: "show", Season: 1, Episode: 1},
			{File: siloapi.File{Path: "/vfs/Show/Season 02/b.mkv"}, ItemID: "show", Season: 2, Episode: 1},
			{File: siloapi.File{Path: "/vfs/Other/Season 01/c.mkv"}, ItemID: "other", Season: 1, Episode: 1},
		},
	}
	targets := seasonTargets(inv, item)
	if len(targets) != 2 || len(targets[1].Paths) != 1 || targets[1].Paths[0] != "/vfs/Show/Season 02/b.mkv" {
		t.Fatalf("targets = %#v", targets)
	}
}

// Settings that name deleted libraries warn once per run, and AnimeThemes
// does not fall back to every series library.
func TestUnknownAnimeLibrariesWarn(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "series")
	silo.addSeries("series:anime", "Anime Show", "1")
	stage := stagedAudio(t)
	general := &fakeProvider{staged: stage}
	anime := &fakeProvider{staged: stage, claims: map[string]bool{"series:anime": true}}
	s, cfg := newTestServer(t, silo, general, anime)
	var logs strings.Builder
	s.log = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true, Level: hclog.Info})
	cfg.AnimeLibraryIDs = []string{"1", "2"}
	cfg.SkipLibraryIDs = []string{"gone"}
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(ctx, "sync", nil)
	if anime.calls.Load() != 0 || general.calls.Load() != 1 {
		t.Fatalf("AnimeThemes ran for libraries that do not exist: %+v", r)
	}
	notices := strings.Join(r.Notices, "\n")
	for _, want := range []string{"library 1, which does not exist", "library 2, which does not exist", "AnimeThemes will not run"} {
		if !strings.Contains(notices, want) {
			t.Fatalf("notices %q lack %q", notices, want)
		}
	}
	count := func(message string) int {
		n := 0
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["@message"] == message && entry["@level"] == "warn" {
				n++
			}
		}
		return n
	}
	if count("AnimeThemes library does not exist") != 2 || count("no AnimeThemes library exists, so AnimeThemes will not run") != 1 || count("skipped library does not exist") != 1 {
		t.Fatalf("warnings: %s", logs.String())
	}
	// One known anime library is enough to run AnimeThemes; the unknown one
	// still warns.
	silo.addSeries("series:anime2", "Anime Two", "3")
	anime.claims["series:anime2"] = true
	cfg.AnimeLibraryIDs, cfg.SkipLibraryIDs = []string{"lib", "9"}, nil
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r = s.Execute(ctx, "sync", nil)
	notices = strings.Join(r.Notices, "\n")
	if anime.calls.Load() != 1 || !strings.Contains(notices, "library 9, which does not exist") || strings.Contains(notices, "will not run") {
		t.Fatalf("known anime library: calls=%d %+v", anime.calls.Load(), r)
	}
}

// A symlink that appears after the library was read, such as a new Shoko VFS
// episode, is read again instead of refusing the item.
func TestSymlinkAddedDuringRunIsReadAgain(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	item := silo.addMovie("movie:fixture", "Fixture")
	target := filepath.Join(t.TempDir(), "part2.mkv")
	if err := os.WriteFile(target, []byte("video"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(item.dir, "movie.part2.mkv")
	versions := 0
	silo.served = func(path string) {
		if path != "/catalog/items/movie:fixture/versions" {
			return
		}
		if versions++; versions == 1 {
			// Silo lists the new file only after the library was read.
			if err := os.Symlink(target, link); err != nil {
				t.Error(err)
			}
			item.videos = append(item.videos, fakeVideo{id: "movie:fixture-2", path: link, season: -1})
		}
	}
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(ctx, "sync", nil)
	if r.Downloaded != 1 || r.State != "complete" || hasAction(r.Rows, "refused") || hasAction(r.Rows, "deferred") {
		t.Fatalf("new symlink not read again: %+v", r)
	}
}

// A symlink Silo still does not list after a second read waits for the next
// run, and nothing is written beside it.
func TestUnknownSymlinkIsDeferred(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	item := silo.addMovie("movie:fixture", "Fixture")
	if err := os.Symlink(t.TempDir(), filepath.Join(item.dir, "stray")); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(ctx, "sync", nil)
	if r.Downloaded != 0 || p.calls.Load() != 0 || len(r.Rows) != 1 || r.Rows[0].Action != "deferred" || !strings.Contains(r.Rows[0].Reason, "symlink found beneath media owner: stray") {
		t.Fatalf("unknown symlink not deferred: %+v", r)
	}
	if _, err := os.Lstat(filepath.Join(item.dir, "theme-music")); !os.IsNotExist(err) {
		t.Fatalf("wrote beside an unknown symlink: %v", err)
	}
}
