package pluginapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
	"github.com/hashicorp/go-hclog"
)

type fakeProvider struct {
	staged     string
	calls      atomic.Int32
	selectErr  error
	candidates []Candidate
}

func (f *fakeProvider) Select(context.Context, Target) ([]Candidate, error) {
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	if f.candidates != nil {
		return f.candidates, nil
	}
	return []Candidate{{ID: "fixture-1", Title: "Fixture", Extension: "ogg", Provenance: "controlled fixture"}}, nil
}
func (f *fakeProvider) Fetch(context.Context, Candidate) (*provider.StagedAudio, error) {
	f.calls.Add(1)
	return &provider.StagedAudio{Path: f.staged, Format: "mp3"}, nil
}

func TestAutoscanHandshakePublicationAndExactDiscovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	owner := filepath.Join(root, "Movie")
	if e := os.Mkdir(owner, 0755); e != nil {
		t.Fatal(e)
	}
	video := filepath.Join(owner, "movie.mkv")
	os.WriteFile(video, []byte("fixture"), 0644)
	stage := filepath.Join(t.TempDir(), "validated.bin")
	os.WriteFile(stage, []byte("validated fixture audio"), 0600)
	p := &fakeProvider{staged: stage}
	var discovered atomic.Bool
	var failRevalidation atomic.Bool
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("X-Profile-Id") != "primary" {
			t.Error("wrong HTTP credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/catalog/themes/capabilities":
			fmt.Fprint(w, `{"state":"available","allowed":false}`)
		case "/api/v2/libraries":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "1", "type": "movies", "enabled": true, "paths": []string{root}}}})
		case "/api/v2/catalog":
			fmt.Fprint(w, `{"items":[{"content_id":"movie:fixture","type":"movie","title":"Fixture"}],"page":{"has_more":false}}`)
		case "/api/v2/catalog/items/movie:fixture":
			// Revalidation reads the item; fail it while the test asks.
			if failRevalidation.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			v := map[string]any{"content_id": "movie:fixture", "type": "movie", "title": "Fixture"}
			if discovered.Load() {
				entries, _ := os.ReadDir(filepath.Join(owner, "theme-music"))
				var title string
				for _, f := range entries {
					if strings.HasSuffix(f.Name(), ".mp3") {
						title = strings.TrimSuffix(f.Name(), ".mp3")
					}
				}
				v["themes"] = map[string]any{"owner_id": "movie:fixture", "items": []any{map[string]string{"title": title, "id": "1"}}}
			}
			json.NewEncoder(w).Encode(v)
		case "/api/v2/catalog/items/movie:fixture/versions":
			fmt.Fprint(w, `{"items":[{"file_id":"1"}]}`)
		case "/api/v2/admin/items/movie:fixture/files":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "1", "library_id": "1", "file_path": video, "observed_root_path": owner}}})
		case "/api/v2/admin/autoscan/settings":
			fmt.Fprint(w, `{"enabled":true}`)
		case "/api/v2/admin/autoscan/sources":
			fmt.Fprint(w, `{"items":[{"id":"source1","plugin_id":"test.plugin","capability_id":"themes","enabled":true,"delivery_mode":"poll","path_rewrites":[]}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer h.Close()
	s := New(&pluginv1.PluginManifest{PluginId: "test.plugin"}, "general", false, func(Config) (Provider, error) { return p, nil })
	defer s.Close()
	c := Defaults()
	c.BaseURL = h.URL
	c.APIKey = "fixture-secret"
	c.ProfileID = "primary"
	c.StateDir = t.TempDir()
	c.LibraryIDs = []string{"1"}
	c.Policy = destinations.Policy{AllowedRoots: []string{root}}
	c.PreviewOnly = false
	if e := s.ConfigureLocal(ctx, c); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(ctx, "sync")
	if r.Downloaded != 0 || p.calls.Load() != 0 || r.State != "prerequisite_failed" {
		t.Fatalf("wrote before scan readiness: %+v", r)
	}
	first, e := s.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes"})
	if e != nil {
		t.Fatal(e)
	}
	if len(first.Changes) != 0 || first.NextMarker == "" {
		t.Fatal("invalid baseline")
	}
	ready, e := s.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: first.NextMarker})
	if e != nil {
		t.Fatal(e)
	}
	r = s.Execute(ctx, "sync")
	if r.Downloaded != 1 {
		t.Fatalf("publication failed: %+v", r)
	}
	// The source candidate is Ogg, but publication must use the staged MP3
	// extension so the scanner and later discovery agree on the actual file.
	if records := s.engine.Records(); len(records) != 1 || !strings.HasSuffix(records[0].Filename, ".mp3") {
		t.Fatalf("converted audio published with wrong extension: %+v", records)
	}
	batch, e := s.PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "themes", Marker: ready.NextMarker})
	if e != nil || len(batch.Changes) != 1 || batch.Changes[0].SourcePath != owner {
		t.Fatalf("scan event: %+v %v", batch, e)
	}
	r = s.Execute(ctx, "sync")
	if r.Downloaded != 0 || p.calls.Load() != 1 {
		t.Fatalf("repeat downloaded: %+v", r)
	}
	discovered.Store(true)
	failRevalidation.Store(true)
	r = s.Execute(ctx, "reconcile")
	if r.State != "needs_attention" || len(r.Errors) == 0 {
		t.Fatalf("temporary read failure hidden: %+v", r)
	}
	for _, record := range s.engine.Records() {
		if record.Status == "stale" || record.Status == "protected" {
			t.Fatalf("temporary API failure permanently froze file: %+v", record)
		}
	}
	failRevalidation.Store(false)
	r = s.Execute(ctx, "reconcile")
	if r.Downloaded != 0 || p.calls.Load() != 1 {
		t.Fatal("reconcile downloaded")
	}
	if records := s.engine.Records(); len(records) != 1 || records[0].Status != "discovered" {
		t.Fatalf("exact discovery not recorded: %+v", records)
	}
	jsonResponse, e := s.Handle(ctx, &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status"})
	if e != nil || strings.Contains(string(jsonResponse.Body), "fixture-secret") {
		t.Fatal("credential in report")
	}
}
func TestTaskKeysAndReadOnlyRoutes(t *testing.T) {
	if taskMode("plugin:12:reconcile") != "reconcile" || taskMode("delete") != "" {
		t.Fatal("task routing")
	}
	s := New(&pluginv1.PluginManifest{}, "general", false, nil)
	r, e := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/"})
	if e != nil || r.StatusCode != 405 {
		t.Fatal("accepted write route")
	}
}

func TestRunOutlivesHostDeadline(t *testing.T) {
	release := make(chan struct{})
	var capabilityRequests atomic.Int32
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/catalog/themes/capabilities":
			capabilityRequests.Add(1)
			<-release
			fmt.Fprint(w, `{"state":"available"}`)
		case "/api/v2/libraries":
			fmt.Fprint(w, `{"items":[{"id":"1","type":"movies","enabled":true}]}`)
		case "/api/v2/catalog":
			fmt.Fprint(w, `{"items":[],"page":{"has_more":false}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer h.Close()
	s := New(&pluginv1.PluginManifest{PluginId: "test.plugin"}, "general", false, func(Config) (Provider, error) { return &fakeProvider{}, nil })
	defer s.Close()
	c := Defaults()
	c.BaseURL, c.APIKey, c.ProfileID, c.StateDir = h.URL, "fixture-secret", "primary", t.TempDir()
	c.LibraryIDs = []string{"1"}
	c.ManualRefresh = true
	c.Policy = destinations.Policy{AllowedRoots: []string{t.TempDir()}}
	if e := s.ConfigureLocal(context.Background(), c); e != nil {
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
	// and with preview only on, Download joins Preview.
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
	c.LibraryIDs = []string{"1", "2"}
	c = c.withDefaults(context.Background(), "test.plugin")
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	if c.siloURL != fallbackSiloURL || c.StateDir != filepath.Join(cache, "plugin-state", "test.plugin") {
		t.Fatalf("defaults: %q %q", c.siloURL, c.StateDir)
	}
	if generation(c, siloapi.ScanSource{ID: "s"}) != generation(Config{SourceGeneration: "1"}, siloapi.ScanSource{ID: "s"}) {
		t.Fatal("resolved URL changed the scan generation")
	}
	libs := []siloapi.Library{{ID: "1", Paths: []string{"/media/movies"}}, {ID: "2", Paths: []string{"/media/tv"}}, {ID: "3", Paths: []string{"/media/other"}}}
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
	bare.APIKey, bare.LibraryIDs = "key", []string{"1"}
	if e := bare.withDefaults(context.Background(), "test.plugin").Validate(); e == nil || !strings.Contains(e.Error(), "state_dir") {
		t.Fatalf("missing state_dir accepted: %v", e)
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

func TestMissingLibrariesListsChoices(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/catalog/themes/capabilities":
			fmt.Fprint(w, `{"state":"available"}`)
		case "/api/v2/libraries":
			fmt.Fprint(w, `{"items":[{"id":"3","name":"Shows","type":"series","enabled":true},{"id":"7","name":"Music","type":"music","enabled":true}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer h.Close()
	s := New(&pluginv1.PluginManifest{PluginId: "test.plugin"}, "general", false, func(Config) (Provider, error) { return &fakeProvider{}, nil })
	defer s.Close()
	c := Defaults()
	c.BaseURL, c.APIKey, c.ProfileID, c.StateDir, c.ManualRefresh = h.URL, "key", "primary", t.TempDir(), true
	if e := s.ConfigureLocal(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	r := s.Execute(context.Background(), "preview")
	if r.State != "prerequisite_failed" || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], `"3" (Shows, series)`) || strings.Contains(r.Errors[0], "Music") {
		t.Fatalf("library choices: %+v", r)
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

func TestReconcileSkipsLibraryReadAndLogsJSON(t *testing.T) {
	var catalogReads atomic.Int32
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/catalog/themes/capabilities":
			fmt.Fprint(w, `{"state":"available"}`)
		case "/api/v2/libraries":
			fmt.Fprint(w, `{"items":[{"id":"1","name":"Movies","type":"movies","enabled":true}]}`)
		case "/api/v2/catalog":
			catalogReads.Add(1)
			fmt.Fprint(w, `{"items":[],"page":{"has_more":false}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer h.Close()
	s := New(&pluginv1.PluginManifest{PluginId: "test.plugin"}, "general", false, func(Config) (Provider, error) { return &fakeProvider{}, nil })
	defer s.Close()
	var logs strings.Builder
	s.log = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true, Level: hclog.Info})
	c := Defaults()
	c.BaseURL, c.APIKey, c.ProfileID, c.StateDir, c.ManualRefresh = h.URL, "fixture-secret", "primary", t.TempDir(), true
	c.LibraryIDs = []string{"1"}
	if e := s.ConfigureLocal(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if r := s.Execute(context.Background(), "reconcile"); r.State != "complete" || catalogReads.Load() != 0 {
		t.Fatalf("reconcile read the library: %d reads, %+v", catalogReads.Load(), r)
	}
	s.Execute(context.Background(), "preview")
	if catalogReads.Load() != 1 {
		t.Fatalf("preview read the library %d times", catalogReads.Load())
	}
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

// Silo's settings form saves JSON settings as text.
func TestSettingsFromFormText(t *testing.T) {
	settings := map[string]any{
		"api_key":           "key",
		"library_ids":       ` ["3", "4"] `,
		"exclude_items":     "5, 6 7",
		"anime_library_ids": "",
		"destinations":      `{"path_mappings":[{"server":"/media","local":"/mnt/media"}]}`,
		"provider":          `{"manage_tools":false}`,
	}
	if e := decodeFormText(settings); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(settings)
	c := Defaults()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&c); e != nil {
		t.Fatal(e)
	}
	if !slices.Equal(c.LibraryIDs, []string{"3", "4"}) || !slices.Equal(c.ExcludeItems, []string{"5", "6", "7"}) || c.AnimeLibraryIDs != nil {
		t.Fatalf("lists: %q %q %q", c.LibraryIDs, c.ExcludeItems, c.AnimeLibraryIDs)
	}
	if len(c.Policy.Mounts) != 1 || c.Policy.Mounts[0].Local != "/mnt/media" || string(c.Provider) != `{"manage_tools":false}` {
		t.Fatalf("objects: %+v %s", c.Policy, c.Provider)
	}
	if e := decodeFormText(map[string]any{"destinations": `{"allowed_roots":`}); e == nil || !strings.Contains(e.Error(), "destinations") {
		t.Fatalf("bad JSON accepted: %v", e)
	}
}
