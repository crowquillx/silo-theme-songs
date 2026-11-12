package animethemes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// gqlServer answers each GraphQL operation with the handler named after it.
func gqlServer(t *testing.T, calls *atomic.Int32, ops map[string]func(gqlRequest) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req gqlRequest
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Errorf("not a GraphQL POST: %s %s", r.Method, r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for name, handle := range ops {
			if strings.Contains(req.Query, "query "+name+"(") {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, handle(req))
				return
			}
		}
		t.Errorf("unexpected query: %s", req.Query)
		w.WriteHeader(http.StatusBadRequest)
	}))
}

func TestExactLookupPaginationCacheAndSelection(t *testing.T) {
	var calls atomic.Int32
	server := gqlServer(t, &calls, map[string]func(gqlRequest) string{
		"Lookup": func(r gqlRequest) string {
			if ids := fmt.Sprint(r.Variables["ids"]); ids != "[9304]" || r.Variables["n"] != float64(2) || !strings.Contains(r.Query, "findAnimeByExternalSite(site: ANIDB, id: $ids)") {
				t.Errorf("wrong exact lookup: ids=%s vars=%v", ids, r.Variables)
			}
			// Anime 10 fills the page of two themes, theme 2 fills its page
			// of two entries, and entry 2 has a second page of videos.
			return `{"data":{"findAnimeByExternalSite":[
				{"id":10,"title":{"romaji":"match"},"resources":{"pageInfo":{"hasNextPage":false},"nodes":[{"site":"ANIDB","externalId":9304}]},"animethemes":[
					{"id":2,"type":"OP","sequence":1,"slug":"OP1","animethemeentries":[
						{"id":1,"version":1,"nsfw":false,"spoiler":true,"videos":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":1,"overlap":"NONE","nc":false,"audio":{"id":1,"link":"https://cdn.example/one.ogg","basename":"one.ogg","filename":"one","mimetype":"audio/ogg","size":10}}]}},
						{"id":2,"version":2,"nsfw":false,"spoiler":false,"videos":{"pageInfo":{"hasNextPage":true,"endCursor":"MQ=="},"nodes":[{"id":2,"overlap":"NONE","nc":true,"audio":{"id":2,"link":"https://cdn.example/two.ogg","basename":"two.ogg","filename":"two","mimetype":"audio/ogg","size":10}}]}}]},
					{"id":3,"type":"IN","sequence":null,"slug":"IN","animethemeentries":[]}]},
				{"id":99,"title":{"romaji":"other"},"resources":{"pageInfo":{"hasNextPage":false},"nodes":[{"site":"ANIDB","externalId":555}]},"animethemes":[]}]}}`
		},
		"Themes": func(r gqlRequest) string {
			if fmt.Sprint(r.Variables["ids"]) != "[9304]" || r.Variables["page"] != float64(2) {
				t.Errorf("wrong theme page: %v", r.Variables)
			}
			return `{"data":{"findAnimeByExternalSite":[{"id":10,"animethemes":[{"id":5,"type":"ED","sequence":1,"slug":"ED","animethemeentries":[]}]},{"id":20,"animethemes":[{"id":6,"type":"ED","sequence":2,"slug":"ED2","animethemeentries":[]}]}]}}`
		},
		"Entries": func(r gqlRequest) string {
			if r.Variables["theme"] != float64(2) || r.Variables["page"] != float64(2) {
				t.Errorf("wrong entry page: %v", r.Variables)
			}
			return `{"data":{"animethemePagination":{"data":[{"id":2,"animethemeentries":[]}]}}}`
		},
		"Videos": func(r gqlRequest) string {
			if r.Variables["entry"] != float64(2) || r.Variables["after"] != "MQ==" {
				t.Errorf("wrong video page: %v", r.Variables)
			}
			return `{"data":{"animethemeentryPagination":{"data":[{"id":2,"videos":{"pageInfo":{"hasNextPage":false,"endCursor":"Mg=="},"nodes":[{"id":3,"overlap":"OVER","nc":false,"audio":{"id":3,"link":"https://cdn.example/three.ogg","basename":"three.ogg","filename":"three","mimetype":"audio/ogg","size":10}}]}}]}}}`
		},
	})
	defer server.Close()
	c := NewClient(server.Client())
	c.BaseURL = server.URL
	c.PageSize = 2
	got, e := c.LookupAniDB(context.Background(), []int{9304, 9304})
	if e != nil {
		t.Fatal(e)
	}
	if len(got[9304]) != 1 || got[9304][0].Name != "match" || len(got[9304][0].Themes) != 3 || len(got[9304][0].Themes[0].Entries[1].Videos) != 2 {
		t.Fatalf("lookup: %+v", got)
	}
	if v := got[9304][0].Themes[0].Entries[1].Version; v == nil || *v != 2 {
		t.Fatalf("entry version: %v", v)
	}
	if calls.Load() != 4 {
		t.Fatalf("calls=%d, want lookup plus three pages", calls.Load())
	}
	again, e := c.LookupAniDB(context.Background(), []int{9304})
	if e != nil || len(again[9304]) != 1 || calls.Load() != 4 {
		t.Fatalf("cache: %v %+v calls=%d", e, again, calls.Load())
	}
	selection := Select(got[9304], Options{OP: true, ED: true})
	if len(selection.Candidates) != 1 || selection.Candidates[0].AudioID != 2 || selection.Candidates[0].Title != "OP1" {
		t.Fatalf("selection: %+v", selection)
	}
	if len(selection.Omissions) < 2 {
		t.Fatalf("omissions: %+v", selection)
	}
	if all := Select(got[9304], Options{OP: true, AllDistinct: true, AllowOverlap: true}); len(all.Candidates) != 2 {
		t.Fatalf("the second video page was not selectable: %+v", all)
	}
}

// An AniDB ID with no anime is cached for the negative TTL only.
func TestNegativeCacheExpires(t *testing.T) {
	var calls atomic.Int32
	srv := gqlServer(t, &calls, map[string]func(gqlRequest) string{
		"Lookup": func(gqlRequest) string { return `{"data":{"findAnimeByExternalSite":[]}}` },
	})
	defer srv.Close()
	c := NewClient(srv.Client())
	c.BaseURL = srv.URL
	c.NegativeTTL = time.Millisecond
	for i := 0; i < 2; i++ {
		if got, e := c.LookupAniDB(context.Background(), []int{4}); e != nil || len(got[4]) != 0 {
			t.Fatalf("lookup: %v %v", got, e)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("negative entry outlived its TTL: calls=%d", calls.Load())
	}
}

func TestBatchesHundredIDs(t *testing.T) {
	var calls atomic.Int32
	sizes := make(chan int, 4)
	srv := gqlServer(t, &calls, map[string]func(gqlRequest) string{
		"Lookup": func(r gqlRequest) string {
			ids, _ := r.Variables["ids"].([]any)
			sizes <- len(ids)
			return `{"data":{"findAnimeByExternalSite":[]}}`
		},
	})
	defer srv.Close()
	c := NewClient(srv.Client())
	c.BaseURL = srv.URL
	ids := make([]int, 150)
	for i := range ids {
		ids[i] = i + 1
	}
	if _, e := c.LookupAniDB(context.Background(), ids); e != nil {
		t.Fatal(e)
	}
	close(sizes)
	got := []int{}
	for n := range sizes {
		got = append(got, n)
	}
	if fmt.Sprint(got) != "[100 50]" {
		t.Fatalf("batches = %v", got)
	}
}

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
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"data":{"findAnimeByExternalSite":null}}`)
		case 2:
			fmt.Fprint(w, `{"errors":[{"message":"Internal server error"}],"data":{"findAnimeByExternalSite":[]}}`)
		case 3:
			fmt.Fprint(w, `{"message":"upstream error"}`)
		default:
			fmt.Fprint(w, `{"data":{"findAnimeByExternalSite":[]}}`)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	c.BaseURL = srv.URL
	for _, want := range []string{"missing anime collection", "query refused: Internal server error", "missing data"} {
		if _, err := c.LookupAniDB(context.Background(), []int{9304}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("accepted malformed success response: %v, want %q", err, want)
		}
	}
	if _, err := c.LookupAniDB(context.Background(), []int{9304}); err != nil || calls.Load() != 4 {
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

func TestRateRetryAndPaginationBounds(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
			case 2:
				// The video cursor never advances.
				fmt.Fprint(w, `{"data":{"findAnimeByExternalSite":[{"id":1,"resources":{"pageInfo":{"hasNextPage":false},"nodes":[{"site":"ANIDB","externalId":1}]},"animethemes":[{"id":1,"type":"OP","slug":"OP1","animethemeentries":[{"id":1,"version":1,"videos":{"pageInfo":{"hasNextPage":true,"endCursor":"MQ=="},"nodes":[]}}]}]}]}}`)
			default:
				fmt.Fprint(w, `{"data":{"animethemeentryPagination":{"data":[{"id":1,"videos":{"pageInfo":{"hasNextPage":true,"endCursor":"MQ=="},"nodes":[]}}]}}}`)
			}
		}))
		c := NewClient(srv.Client())
		c.BaseURL = srv.URL
		if _, e := c.LookupAniDB(context.Background(), []int{1}); e == nil || !strings.Contains(e.Error(), "pagination cycle") {
			t.Fatalf("HTTP %d: expected pagination cycle refusal, got %v", status, e)
		}
		if calls.Load() != 3 {
			t.Fatalf("HTTP %d: calls=%d", status, calls.Load())
		}
		srv.Close()
	}

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Every page of themes is full.
		fmt.Fprint(w, `{"data":{"findAnimeByExternalSite":[{"id":1,"resources":{"pageInfo":{"hasNextPage":false},"nodes":[{"site":"ANIDB","externalId":1}]},"animethemes":[{"id":1,"type":"OP","slug":"OP1","animethemeentries":[]}]}]}}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	c.BaseURL = srv.URL
	c.PageSize, c.MaxPages = 1, 2
	if _, e := c.LookupAniDB(context.Background(), []int{1}); e == nil || !strings.Contains(e.Error(), "pagination limit") {
		t.Fatalf("expected bounded pagination refusal, got %v", e)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d, want the lookup and two pages", calls.Load())
	}
}

func TestBaseURLMustUseHTTPS(t *testing.T) {
	c := NewClient(nil)
	c.BaseURL = "http://graphql.animethemes.moe/"
	if _, e := c.LookupAniDB(context.Background(), []int{1}); e == nil || !strings.Contains(e.Error(), "HTTPS") {
		t.Fatalf("plain HTTP endpoint accepted: %v", e)
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ids := []int{23, 4563, 6662, 9304}
	got, e := NewClient(nil).LookupAniDB(ctx, ids)
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range ids {
		if len(got[id]) == 0 {
			t.Fatalf("AniDB %d currently has no AnimeThemes anime result", id)
		}
		sel := Select(got[id], Options{OP: true, ED: true})
		if len(sel.Candidates) == 0 {
			t.Fatalf("AniDB %d: no candidates: %+v", id, sel.Omissions)
		}
		for _, c := range sel.Candidates {
			if u, _ := url.Parse(c.URL); u == nil || u.Host != "a.animethemes.moe" {
				t.Errorf("AniDB %d: audio host changed: %s", id, c.URL)
			}
		}
		first := sel.Candidates[0]
		t.Logf("AniDB %d: %s, %d candidates, %d omissions; first %s %s%d %s (%s, %d bytes)", id, got[id][0].Name, len(sel.Candidates), len(sel.Omissions), first.Title, first.Type, first.Sequence, first.URL, first.MIMEType, first.Size)
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
