package anime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		id := r.URL.Query().Get("filter[resource][external_id]")
		if id != "10206,10835" {
			t.Errorf("unexpected AniDB filter %q", id)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"anime":[{"id":1,"resources":[{"site":"AniDB","external_id":10206}],"animethemes":[{"id":5,"type":"OP","sequence":1,"slug":"op-one","animethemeentries":[{"id":6,"videos":[{"id":7,"overlap":"None","audio":{"id":7,"link":"https://a.animethemes.moe/op.ogg","filename":"op.ogg","size":100}}]}]}]},{"id":2,"resources":[{"site":"AniDB","external_id":10835}],"animethemes":[{"id":8,"type":"ED","sequence":1,"slug":"ed-one","animethemeentries":[{"id":9,"videos":[{"id":10,"overlap":"None","audio":{"id":11,"link":"https://a.animethemes.moe/ed.ogg","filename":"ed.ogg","size":100}}]}]}]}],"links":{"next":null}}`))
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
