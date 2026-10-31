package animethemes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExactLookupPaginationCacheAndSelection(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("filter[resource][external_id]") != "9304" || r.URL.Query().Get("filter[resource][site]") != "aniDB" {
			t.Errorf("wrong exact filter: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page[number]") == "2" {
			fmt.Fprint(w, `{"anime":[{"id":20,"name":"match 2","resources":[{"site":"AniDB","external_id":9304}],"animethemes":[{"id":5,"type":"ED","sequence":1,"animethemeentries":[]}]}],"links":{"next":null}}`)
			return
		}
		fmt.Fprintf(w, `{"anime":[{"id":10,"name":"match","resources":[{"site":"AniDB","external_id":9304}],"animethemes":[{"id":2,"type":"OP","sequence":1,"slug":"op1","animethemeentries":[{"id":1,"spoiler":true,"videos":[{"id":1,"overlap":"None","audio":{"id":1,"link":"https://cdn.example/one.ogg","filename":"one.ogg","size":10}}]},{"id":2,"videos":[{"id":2,"overlap":"None","nc":true,"audio":{"id":2,"link":"https://cdn.example/two.ogg","filename":"two.ogg","size":10}},{"id":3,"overlap":"Over","audio":{"id":3,"link":"https://cdn.example/three.ogg","filename":"three.ogg","size":10}}]}]}]},{"id":99,"resources":[{"site":"AniDB","external_id":555}]}],"links":{"next":%q}}`, serverURL(r)+"?"+r.URL.Query().Encode()+"&page%5Bnumber%5D=2")
	}))
	defer server.Close()
	c := NewClient(server.Client())
	c.BaseURL = server.URL
	got, e := c.LookupAniDB(context.Background(), []int{9304, 9304})
	if e != nil {
		t.Fatal(e)
	}
	if len(got[9304]) != 2 {
		t.Fatalf("lookup: %+v", got)
	}
	again, e := c.LookupAniDB(context.Background(), []int{9304})
	if e != nil || len(again[9304]) != 2 || calls.Load() != 2 {
		t.Fatalf("cache: %v %+v calls=%d", e, again, calls.Load())
	}
	selection := Select(got[9304], Options{OP: true, ED: true})
	if len(selection.Candidates) != 1 || selection.Candidates[0].AudioID != 2 {
		t.Fatalf("selection: %+v", selection)
	}
	if len(selection.Omissions) < 2 {
		t.Fatalf("omissions: %+v", selection)
	}
}

func serverURL(r *http.Request) string { return "http://" + r.Host + r.URL.Path }

func TestMissingAudioAndDistinctDedup(t *testing.T) {
	audio := &Audio{ID: 10, Link: "https://cdn.example/a.ogg", Filename: "a.ogg", Size: 100}
	a := Anime{ID: 1, Resources: []Resource{{ExternalID: 1, Site: "AniDB"}}, Themes: []Theme{{ID: 5, Type: "OP", Entries: []Entry{{ID: 1, Videos: []Video{{ID: 1, Overlap: "None", Audio: audio}, {ID: 2, Overlap: "None", Audio: audio}, {ID: 3, Overlap: "None"}}}}}}}
	got := Select([]Anime{a}, Options{OP: true, AllDistinct: true})
	if len(got.Candidates) != 1 || got.Candidates[0].AudioID != 10 {
		t.Fatalf("dedup: %+v", got)
	}
	if !strings.Contains(got.Omissions[0].Reason, "audio") {
		t.Fatalf("omission: %+v", got.Omissions)
	}
}

func TestMissingCollectionIsNotCachedAsNoMatch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"message":"upstream error"}`)
			return
		}
		fmt.Fprint(w, `{"anime":[],"links":{"next":null}}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	c.BaseURL = srv.URL
	if _, err := c.LookupAniDB(context.Background(), []int{9304}); err == nil || !strings.Contains(err.Error(), "missing anime collection") {
		t.Fatalf("accepted malformed success response: %v", err)
	}
	if _, err := c.LookupAniDB(context.Background(), []int{9304}); err != nil || calls.Load() != 2 {
		t.Fatalf("malformed response was cached: calls=%d error=%v", calls.Load(), err)
	}
}

func TestSelectionRequiresStableResponseIdentities(t *testing.T) {
	validAudio := &Audio{ID: 7, Link: "https://cdn.example/song.ogg", Filename: "song.ogg", Size: 100}
	makeAnime := func(animeID, themeID, entryID, videoID int) Anime {
		return Anime{ID: animeID, Resources: []Resource{{ExternalID: 9304, Site: "AniDB"}}, Themes: []Theme{{ID: themeID, Type: "OP", Entries: []Entry{{ID: entryID, Videos: []Video{{ID: videoID, Overlap: "None", Audio: validAudio}}}}}}}
	}
	anime := []Anime{makeAnime(0, 1, 1, 1), makeAnime(1, 0, 1, 1), makeAnime(1, 2, 0, 1), makeAnime(1, 3, 1, 0), makeAnime(1, 4, 1, 1)}
	got := Select(anime, Options{OP: true})
	if len(got.Candidates) != 1 || got.Candidates[0].ThemeID != 4 {
		t.Fatalf("missing IDs became downloadable candidates: %+v", got.Candidates)
	}
}

func TestRateRetryAndUnsafePagination(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"anime":[],"links":{"next":"https://unrelated.example/anime/?page=2"}}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	c.BaseURL = srv.URL
	if _, e := c.LookupAniDB(context.Background(), []int{1}); e == nil || !strings.Contains(e.Error(), "unsafe next-page") {
		t.Fatalf("expected bounded pagination refusal, got %v", e)
	}
	if calls.Load() != 2 {
		t.Fatalf("retry calls=%d", calls.Load())
	}
}

func TestRateCooldownSurvivesNewAnimeClient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	first := NewClient(srv.Client())
	first.BaseURL = srv.URL
	if _, err := first.LookupAniDB(context.Background(), []int{9304}); err == nil || !strings.Contains(err.Error(), "retry later") {
		t.Fatalf("first throttle: %v", err)
	}
	second := NewClient(srv.Client())
	second.BaseURL = srv.URL
	started := time.Now()
	if _, err := second.LookupAniDB(context.Background(), []int{9305}); err == nil || !strings.Contains(err.Error(), "retry later") {
		t.Fatalf("second throttle: %v", err)
	}
	if calls.Load() != 1 || time.Since(started) > time.Second {
		t.Fatalf("new client sent request during cooldown: calls=%d elapsed=%s", calls.Load(), time.Since(started))
	}
}

func TestLiveAnimeThemesAniDB(t *testing.T) {
	if os.Getenv("LIVE_ANIMETHEMES") != "1" {
		t.Skip("set LIVE_ANIMETHEMES=1 for upstream availability check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	got, e := NewClient(nil).LookupAniDB(ctx, []int{9304})
	if e != nil {
		t.Fatal(e)
	}
	if len(got[9304]) == 0 {
		t.Fatal("AniDB 9304 currently has no AnimeThemes anime result")
	}
}

// The API now sends the extension in basename and omits it from filename.
func TestAudioExtensionFromBasename(t *testing.T) {
	for _, c := range []struct {
		audio Audio
		want  string
	}{
		{Audio{ID: 1, Size: 1, Link: "https://a.animethemes.moe/CowboyBebop-OP1.ogg", Basename: "CowboyBebop-OP1.ogg", Filename: "CowboyBebop-OP1"}, "ogg"},
		{Audio{ID: 1, Size: 1, Link: "https://a.animethemes.moe/one.ogg", Filename: "one.ogg"}, "ogg"},
		{Audio{ID: 1, Size: 1, Link: "https://a.animethemes.moe/one.ogg", Filename: "one"}, ""},
		{Audio{ID: 1, Size: 1, Link: "https://a.animethemes.moe/one.ogg", Basename: "one.mp3", Filename: "one"}, ""},
	} {
		if got := audioExt(c.audio); got != c.want {
			t.Errorf("audioExt(%+v) = %q, want %q", c.audio, got, c.want)
		}
	}
}
