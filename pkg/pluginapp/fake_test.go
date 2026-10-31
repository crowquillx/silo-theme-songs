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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
)

// fakeSilo serves the parts of Silo's API the plugin uses, for one library
// of movies or series folders beneath root.
type fakeSilo struct {
	*httptest.Server
	t       *testing.T
	root    string
	libType string

	mu sync.Mutex
	// items are listed in the catalog in order.
	items []*fakeItem
	// owners maps theme owner IDs (items and seasons) to their folders.
	owners map[string]string
	// visible lists owners whose themes Silo has scanned.
	visible map[string]bool
	scans   []string
	// scanStatus answers POST /scan; 0 means 202.
	scanStatus int
	// scanDiscovers makes a scan request list the theme at once.
	scanDiscovers bool
	apiCalls      atomic.Int32
}

type fakeItem struct {
	id, kind, title, tvdb string
	dir                   string
	videos                []fakeVideo
}

type fakeVideo struct {
	id, path        string
	season, episode int
}

func newFakeSilo(t *testing.T, libType string) *fakeSilo {
	t.Helper()
	f := &fakeSilo{t: t, root: t.TempDir(), libType: libType, owners: map[string]string{}, visible: map[string]bool{}, scanDiscovers: true}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// addMovie creates root/<title>/movie.mkv.
func (f *fakeSilo) addMovie(id, title string) *fakeItem {
	f.t.Helper()
	dir := filepath.Join(f.root, title)
	video := filepath.Join(dir, "movie.mkv")
	if err := os.MkdirAll(dir, 0755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(video, []byte("video"), 0644); err != nil {
		f.t.Fatal(err)
	}
	item := &fakeItem{id: id, kind: "movie", title: title, dir: dir, videos: []fakeVideo{{id: id + "-file", path: video, season: -1}}}
	f.mu.Lock()
	f.items = append(f.items, item)
	f.owners[id] = dir
	f.mu.Unlock()
	return item
}

// addSeries creates root/<title>/Season 01/e1.mkv.
func (f *fakeSilo) addSeries(id, title, tvdb string) *fakeItem {
	f.t.Helper()
	dir := filepath.Join(f.root, title)
	season := filepath.Join(dir, "Season 01")
	video := filepath.Join(season, "e1.mkv")
	if err := os.MkdirAll(season, 0755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(video, []byte("video"), 0644); err != nil {
		f.t.Fatal(err)
	}
	item := &fakeItem{id: id, kind: "series", title: title, tvdb: tvdb, dir: dir, videos: []fakeVideo{{id: id + "-e1", path: video, season: 1, episode: 1}}}
	f.mu.Lock()
	f.items = append(f.items, item)
	f.owners[id] = dir
	f.owners[id+":s1"] = season
	f.mu.Unlock()
	return item
}

func (f *fakeSilo) scanned() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.scans...)
}

func (f *fakeSilo) item(id string) *fakeItem {
	for _, item := range f.items {
		if item.id == id {
			return item
		}
	}
	return nil
}

func (f *fakeSilo) serve(w http.ResponseWriter, r *http.Request) {
	f.apiCalls.Add(1)
	if r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("X-Profile-Id") != "primary" {
		f.t.Errorf("wrong credentials for %s", r.URL.Path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v2")
	send := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodPost && path == "/scan":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.scans = append(f.scans, body["path"])
		if f.scanStatus != 0 {
			w.WriteHeader(f.scanStatus)
			return
		}
		if f.scanDiscovers {
			owner := filepath.Dir(filepath.Dir(body["path"]))
			for id, dir := range f.owners {
				if dir == owner {
					f.visible[id] = true
				}
			}
		}
		w.WriteHeader(http.StatusAccepted)
		send(map[string]string{"status": "accepted", "mode": "file", "library_id": body["library_id"]})
	case path == "/catalog/themes/capabilities":
		send(map[string]string{"state": "available"})
	case path == "/libraries":
		send(map[string]any{"items": []any{
			map[string]any{"id": "lib", "name": "Library", "type": f.libType, "enabled": true, "paths": []string{f.root}},
			map[string]any{"id": "music", "name": "Music", "type": "music", "enabled": true, "paths": []string{"/music"}},
		}})
	case path == "/catalog":
		rows := []any{}
		for _, item := range f.items {
			rows = append(rows, map[string]any{"content_id": item.id, "type": item.kind, "title": item.title})
		}
		send(map[string]any{"items": rows, "page": map[string]any{"has_more": false}})
	case strings.HasPrefix(path, "/catalog/items/") && strings.HasSuffix(path, "/versions"):
		item := f.item(strings.TrimSuffix(strings.TrimPrefix(path, "/catalog/items/"), "/versions"))
		send(map[string]any{"items": []any{map[string]string{"file_id": item.videos[0].id}}})
	case strings.HasPrefix(path, "/catalog/items/"):
		id := strings.TrimPrefix(path, "/catalog/items/")
		v := map[string]any{"content_id": id}
		if item := f.item(id); item != nil {
			v["type"], v["title"], v["tvdb_id"] = item.kind, item.title, item.tvdb
		}
		if f.visible[id] {
			titles := []any{}
			entries, _ := os.ReadDir(filepath.Join(f.owners[id], "theme-music"))
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".mp3") {
					titles = append(titles, map[string]string{"id": e.Name(), "title": strings.TrimSuffix(e.Name(), ".mp3")})
				}
			}
			v["themes"] = map[string]any{"owner_id": id, "items": titles}
		}
		send(v)
	case strings.HasPrefix(path, "/admin/items/") && strings.HasSuffix(path, "/files"):
		item := f.item(strings.TrimSuffix(strings.TrimPrefix(path, "/admin/items/"), "/files"))
		files := []any{}
		for _, v := range item.videos {
			files = append(files, map[string]string{"id": v.id, "library_id": "lib", "file_path": v.path, "observed_root_path": item.dir})
		}
		send(map[string]any{"items": files, "page": map[string]any{"has_more": false}})
	case strings.HasPrefix(path, "/catalog/series/") && strings.HasSuffix(path, "/seasons"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/catalog/series/"), "/seasons")
		send(map[string]any{"items": []any{map[string]any{"content_id": id + ":s1", "season_number": 1}}})
	case strings.HasPrefix(path, "/catalog/series/") && strings.HasSuffix(path, "/seasons/1/episodes"):
		item := f.item(strings.TrimSuffix(strings.TrimPrefix(path, "/catalog/series/"), "/seasons/1/episodes"))
		send(map[string]any{"items": []any{map[string]any{"season_number": 1, "episode_number": 1, "files": []any{map[string]string{"file_id": item.videos[0].id}}}}})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

type fakeProvider struct {
	staged     string
	calls      atomic.Int32
	selectErr  error
	candidates []Candidate
	// claims lists the series the provider claims, for AnimeThemes fakes.
	claims map[string]bool
}

func (f *fakeProvider) Select(_ context.Context, t Target) ([]Candidate, error) {
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	if f.claims != nil && t.Season == nil {
		return nil, nil
	}
	if f.candidates != nil {
		return f.candidates, nil
	}
	return []Candidate{{ID: "fixture-" + t.Item.ID, Title: "Fixture " + t.Item.Title, Extension: "ogg", Provenance: "controlled fixture"}}, nil
}

func (f *fakeProvider) Fetch(context.Context, Candidate) (*provider.StagedAudio, error) {
	f.calls.Add(1)
	return &provider.StagedAudio{Path: f.staged, Format: "mp3"}, nil
}

func (f *fakeProvider) Claims(_ context.Context, targets []Target) (bool, error) {
	return f.claims[targets[0].Item.ID], nil
}

func stagedAudio(t *testing.T) string {
	t.Helper()
	stage := filepath.Join(t.TempDir(), "audio.bin")
	if err := os.WriteFile(stage, []byte(fmt.Sprintf("validated audio fixture %d", time.Now().UnixNano())), 0600); err != nil {
		t.Fatal(err)
	}
	return stage
}

// newTestServer returns a server for silo with the given providers; anime
// may be nil. The settings use every movie and series library.
func newTestServer(t *testing.T, silo *fakeSilo, general, anime Provider) (*Server, Config) {
	t.Helper()
	var animeFactory Factory
	if anime != nil {
		animeFactory = func(Config) (Provider, error) { return anime, nil }
	}
	s := New(&pluginv1.PluginManifest{PluginId: "test.plugin"}, func(Config) (Provider, error) { return general, nil }, animeFactory)
	s.confirmWaits = []time.Duration{5 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { _ = s.Close() })
	cfg := Defaults()
	cfg.BaseURL, cfg.APIKey, cfg.ProfileID, cfg.StateDir = silo.URL, "fixture-secret", "primary", t.TempDir()
	return s, cfg
}
