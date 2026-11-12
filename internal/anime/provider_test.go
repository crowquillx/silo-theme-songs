package anime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crowquillx/silo-theme-songs/internal/mapping"
	"github.com/crowquillx/silo-theme-songs/pkg/pluginapp"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
)

func TestAdapterCachedMappingAndSeasonSelection(t *testing.T) {
	dir := t.TempDir()
	mapping := []byte(`{"$meta":{"schema_version":"3.0.3"},"tvdb_show:262954:s2":{"anidb:10206:R":{"1-24":"1-24"},"anidb:10835:R":{"25-48":"1-24"}}}`)
	if e := os.WriteFile(filepath.Join(dir, "anibridge-v3.json"), mapping, 0600); e != nil {
		t.Fatal(e)
	}
	cfg, _ := json.Marshal(ProviderConfig{SelectionOverrides: map[string][]string{"show:s2": {"animethemes-5-7"}}})
	p, e := New(pluginapp.Config{StateDir: dir, FFprobe: "ffprobe", AnimeProvider: cfg})
	if e != nil {
		t.Fatal(e)
	}
	a := p.(*Adapter)
	a.lastRefresh = time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables struct {
				IDs []int `json:"ids"`
			} `json:"variables"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || fmt.Sprint(req.Variables.IDs) != "[10206 10835]" {
			t.Errorf("unexpected AniDB lookup %s %v", r.Method, req.Variables.IDs)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"findAnimeByExternalSite":[{"id":1,"title":{"romaji":"one"},"resources":{"pageInfo":{"hasNextPage":false},"nodes":[{"site":"ANIDB","externalId":10206}]},"animethemes":[{"id":5,"type":"OP","sequence":1,"slug":"op-one","animethemeentries":[{"id":6,"version":1,"nsfw":false,"spoiler":false,"videos":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":7,"overlap":"NONE","nc":false,"audio":{"id":7,"link":"https://a.animethemes.moe/op.ogg","basename":"op.ogg","filename":"op","mimetype":"audio/ogg","size":100}}]}}]}]},{"id":2,"title":{"romaji":"two"},"resources":{"pageInfo":{"hasNextPage":false},"nodes":[{"site":"ANIDB","externalId":10835}]},"animethemes":[{"id":8,"type":"ED","sequence":1,"slug":"ed-one","animethemeentries":[{"id":9,"version":1,"nsfw":false,"spoiler":false,"videos":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":10,"overlap":"NONE","nc":false,"audio":{"id":11,"link":"https://a.animethemes.moe/ed.ogg","basename":"ed.ogg","filename":"ed","mimetype":"audio/ogg","size":100}}]}}]}]}]}}`))
	}))
	defer srv.Close()
	a.themes.BaseURL = srv.URL
	a.themes.HTTP = srv.Client()
	target := pluginapp.Target{Item: siloapi.Item{ID: "show", TVDB: "262954", Type: "series"}, Season: &siloapi.Season{Number: 2}, Episodes: []int{1}}
	got, e := a.Select(context.Background(), target)
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 1 || got[0].ID != "animethemes-5-7" || got[0].Extension != "ogg" || got[0].Extract {
		t.Fatalf("wrong candidates: %+v", got)
	}
	delete(a.config.SelectionOverrides, "show:s2")
	got, e = a.Select(context.Background(), target)
	if e != nil || len(got) != 2 || got[0].ID != "animethemes-8-11" || got[1].ID != "animethemes-5-7" {
		t.Fatalf("partial media pruned a mapped cour: %+v %v", got, e)
	}
	a.config.SelectionOverrides["show:s2"] = []string{"animethemes-5-999"}
	if _, e := a.Select(context.Background(), target); e == nil {
		t.Fatal("unavailable override was silently ignored")
	}
}

func TestPreflightReportsMissingFFprobe(t *testing.T) {
	p, e := New(pluginapp.Config{StateDir: t.TempDir(), FFprobe: "/nonexistent/ffprobe"})
	if e != nil {
		t.Fatal(e)
	}
	if e = p.(*Adapter).Preflight(context.Background(), pluginapp.Candidate{URL: "https://a.animethemes.moe/test.ogg", Extension: "ogg"}); !provider.IsCode(e, provider.MissingTool) {
		t.Fatalf("expected missing tool, got %v", e)
	}
	if e = p.(*Adapter).Preflight(context.Background(), pluginapp.Candidate{URL: "https://unlisted.example/test.ogg", Extension: "ogg"}); !provider.IsCode(e, provider.UnsafeURL) {
		t.Fatalf("expected unsafe URL, got %v", e)
	}
}

func TestCrossProviderScopeDisagreementIsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`{"$meta":{"schema_version":"3"},"tvdb_show:10:s1":{"anidb:20:R":{"1-12":"1-12"}},"tmdb_show:30:s1":{"anidb:20:S":{"1-12":"1-12"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "anibridge-v3.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := New(pluginapp.Config{StateDir: dir, FFprobe: "ffprobe"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.(*Adapter).resolve(pluginapp.Target{Item: siloapi.Item{ID: "show", Type: "series", TVDB: "10", TMDB: "30"}, Season: &siloapi.Season{Number: 1}})
	if err != nil || got.Status != "ambiguous" {
		t.Fatalf("regular/special disagreement accepted: %+v %v", got, err)
	}
}

func TestClaimsMappedSeriesAndFallback(t *testing.T) {
	dir := t.TempDir()
	mapping := []byte(`{"$meta":{"schema_version":"3.0.3"},"tvdb_show:262954:s2":{"anidb:10206:R":{"1-24":"1-24"}}}`)
	if e := os.WriteFile(filepath.Join(dir, "anibridge-v3.json"), mapping, 0600); e != nil {
		t.Fatal(e)
	}
	cfg, _ := json.Marshal(ProviderConfig{SeriesFallback: map[string][]string{"manual": {"42"}}})
	p, e := New(pluginapp.Config{StateDir: dir, FFprobe: "ffprobe", AnimeProvider: cfg})
	if e != nil {
		t.Fatal(e)
	}
	a := p.(*Adapter)
	a.lastRefresh = time.Now()
	targets := func(id, tvdb string) []pluginapp.Target {
		item := siloapi.Item{ID: id, TVDB: tvdb, Type: "series"}
		return []pluginapp.Target{{Item: item}, {Item: item, Season: &siloapi.Season{Number: 1}}, {Item: item, Season: &siloapi.Season{Number: 2}}}
	}
	for _, c := range []struct {
		id, tvdb string
		want     bool
	}{{"anime", "262954", true}, {"western", "81189", false}, {"manual", "", true}} {
		got, e := a.Claims(context.Background(), targets(c.id, c.tvdb))
		if e != nil || got != c.want {
			t.Errorf("Claims(%s) = %v, %v; want %v", c.id, got, e, c.want)
		}
	}
}

func TestSwitchesOverrideAnimeJSON(t *testing.T) {
	cfg := []byte(`{"op":false,"ed":true,"allow_nsfw":false,"allow_spoiler":true}`)
	yes, no := true, false
	p, e := New(pluginapp.Config{StateDir: t.TempDir(), FFprobe: "ffprobe", AnimeProvider: cfg, AnimeOpenings: &yes, AnimeEndings: &no, AnimeNSFW: &yes})
	if e != nil {
		t.Fatal(e)
	}
	c := p.(*Adapter).config
	if !enabled(c.OP) || enabled(c.ED) || !c.AllowNSFW || !c.AllowSpoiler {
		t.Fatalf("switches did not override JSON: %+v", c)
	}
}

// A Shoko VFS library names every file with its AniDB anime, which identifies
// each season exactly, without TVDB or TMDB IDs or the mapping data.
func TestShokoFileNamesIdentifySeasons(t *testing.T) {
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer unavailable.Close()
	cfg, _ := json.Marshal(ProviderConfig{AniBridgeURL: unavailable.URL, AnimeListsURL: unavailable.URL})
	p, e := New(pluginapp.Config{StateDir: t.TempDir(), FFprobe: "ffprobe", AnimeProvider: cfg})
	if e != nil {
		t.Fatal(e)
	}
	a := p.(*Adapter)
	item := siloapi.Item{ID: "shoko-show", Type: "series"}
	season := func(n int, paths ...string) pluginapp.Target {
		return pluginapp.Target{Item: item, Season: &siloapi.Season{Number: n}, Paths: paths}
	}
	named := func(anime int, episode int) string {
		return fmt.Sprintf("/vfs/Show (2020)/Season 02/Show - S02E%02d [Shoko Group=1] [Shoko Series=7] [AniDB Anime=%d] [Shoko Episode=4%d] [Shoko File=9%d].mkv", episode, anime, episode, episode)
	}

	got, e := a.resolve(season(2, named(1001, 1), named(1001, 2)))
	if e != nil || got.Status != mapping.Resolved || len(got.Matches) != 1 || got.Matches[0].AniDBID != "1001" || got.Matches[0].Source != "filename" {
		t.Fatalf("resolve = %#v, %v", got, e)
	}
	if got, _ := a.resolve(season(2, named(1001, 1), named(1002, 2))); got.Status != mapping.Ambiguous {
		t.Fatalf("mixed anime = %#v, want ambiguous", got)
	}
	if _, ok := fileMatch(season(0, named(1001, 1))); ok {
		t.Fatal("specials were matched to one anime")
	}
	if _, ok := fileMatch(season(1, "/media/Show/Season 01/Show - S01E01.mkv")); ok {
		t.Fatal("a file without the marker was matched")
	}

	// The mapping download fails, yet the Shoko show is still claimed.
	claimed, e := a.Claims(context.Background(), []pluginapp.Target{{Item: item}, season(2, named(1001, 1))})
	if e != nil || !claimed {
		t.Fatalf("Claims = %v, %v", claimed, e)
	}
}
