package pluginapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
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
	var inventoryRequests atomic.Int32
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
			if n := inventoryRequests.Add(1); n >= 2 && n <= 4 && failRevalidation.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"items":[{"content_id":"movie:fixture","type":"movie","title":"Fixture"}],"page":{"has_more":false}}`)
		case "/api/v2/catalog/items/movie:fixture":
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
	inventoryRequests.Store(0)
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
