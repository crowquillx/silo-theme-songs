package mapping

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestJoJoCoursAndPrecedence(t *testing.T) {
	data := []byte(`{"$meta":{"schema_version":"3.0.3"},"tvdb_show:262954:s1":{"anidb:9304:R":{"1-26":"1-26"}},"tvdb_show:262954:s2":{"anidb:10206:R":{"1-24":"1-24"},"anidb:10835:R":{"25-48":"1-24"}},"tmdb_show:45790:s3":{"anidb:11734:R":{"1-39":"1-39"}},"tvdb_show:262954:s4":{"anidb:14173:R":{"1-39":"1-39"}},"tvdb_show:262954:s5":{"anidb:16187:R":{"1-12":"1-12"},"anidb:17286:R":{"13-24":"1-12"},"anidb:17664:R":{"25-38":"1-14"}},"tvdb_show:262954:s0":{"anidb:13812:R":{"1-2":"1-2"}}}`)
	primary, e := ParseAniBridge(data)
	if e != nil {
		t.Fatal(e)
	}
	fallback, e := ParseAnimeLists([]byte(`<anime-list><anime anidbid="999" tvdbid="262954" defaulttvdbseason="2" episodeoffset="0"/></anime-list>`))
	if e != nil {
		t.Fatal(e)
	}
	r := NewResolver(primary, fallback)
	for _, tc := range []struct {
		provider string
		season   int
		ids      string
	}{{"tvdb_show", 1, "9304"}, {"tvdb_show", 2, "10206,10835"}, {"tmdb_show", 3, "11734"}, {"tvdb_show", 4, "14173"}, {"tvdb_show", 5, "16187,17286,17664"}, {"tvdb_show", 0, "13812"}} {
		id := "262954"
		if tc.provider == "tmdb_show" {
			id = "45790"
		}
		got := r.Resolve(Query{Provider: tc.provider, ID: id, Season: tc.season})
		if got.Status != Resolved {
			t.Fatalf("season %d: %+v", tc.season, got)
		}
		ids := []string{}
		for _, m := range got.Matches {
			ids = append(ids, m.AniDBID)
		}
		if strings.Join(ids, ",") != tc.ids {
			t.Fatalf("season %d: %v", tc.season, ids)
		}
	}
	if got := r.Resolve(Query{Provider: "tvdb_show", ID: "262954", Season: 2, Episodes: []int{25}}); len(got.Matches) != 1 || got.Matches[0].AniDBID != "10835" {
		t.Fatalf("offset boundary: %+v", got)
	}
}

func TestFallbackAmbiguityAndOpenOffset(t *testing.T) {
	primary, e := ParseAniBridge([]byte(`{"$meta":{"schema_version":"3"},"tvdb_show:12:s1":{"anidb:1:R":{"1-12":"1-12"},"anidb:2:R":{"10-24":"1-15"}}}`))
	if e != nil {
		t.Fatal(e)
	}
	fallback, e := ParseAnimeLists([]byte(`<anime-list><anime anidbid="3" tvdbid="12" defaulttvdbseason="1" episodeoffset="24"/><anime anidbid="4" tvdbid="12" defaulttvdbseason="2"><mapping-list><mapping anidbseason="1" tvdbseason="2" start="1" end="12" offset="12"/></mapping-list></anime></anime-list>`))
	if e != nil {
		t.Fatal(e)
	}
	r := NewResolver(primary, fallback)
	if got := r.Resolve(Query{Provider: "tvdb_show", ID: "12", Season: 1}); got.Status != Ambiguous || got.Provenance.Version != "3" {
		t.Fatalf("primary ambiguity: %+v", got)
	}
	if got := r.Resolve(Query{Provider: "tvdb_show", ID: "12", Season: 2, Episodes: []int{13}}); got.Status != Resolved || len(got.Matches) != 1 || got.Matches[0].AniDBID != "4" || got.Matches[0].CoverageUnknown {
		t.Fatalf("finite fallback: %+v", got)
	}
	if got := r.Resolve(Query{Provider: "tvdb_show", ID: "12", Season: 1, Episodes: []int{25}, ManualAniDBIDs: []string{"9"}}); got.Matches[0].AniDBID != "9" {
		t.Fatalf("manual: %+v", got)
	}
	if got := NewResolver(nil, fallback).Resolve(Query{Provider: "tvdb_show", ID: "12", Season: 1, Episodes: []int{25}}); !got.Matches[0].CoverageUnknown {
		t.Fatalf("open offset must be uncertain: %+v", got)
	}
}

func TestAnimeListsJoJoOffsetsAndSpecials(t *testing.T) {
	xml := `<anime-list>
 <anime anidbid="9304" tvdbid="262954" defaulttvdbseason="1" tmdbtv="45790" tmdbseason="1"><mapping-list><mapping anidbseason="0" tvdbseason="0" offset="7"/></mapping-list></anime>
 <anime anidbid="10206" tvdbid="262954" defaulttvdbseason="2" tmdbtv="45790" tmdbseason="2"/>
 <anime anidbid="10835" tvdbid="262954" defaulttvdbseason="2" episodeoffset="24" tmdbtv="45790" tmdbseason="2" tmdboffset="24"/>
 <anime anidbid="11734" tvdbid="262954" defaulttvdbseason="3" tmdbtv="45790" tmdbseason="3"/>
 <anime anidbid="14173" tvdbid="262954" defaulttvdbseason="4" tmdbtv="45790" tmdbseason="4"/>
 <anime anidbid="16187" tvdbid="262954" defaulttvdbseason="5" tmdbtv="45790" tmdbseason="5"/>
 <anime anidbid="17286" tvdbid="262954" defaulttvdbseason="5" episodeoffset="12" tmdbtv="45790" tmdbseason="5" tmdboffset="12"/>
 <anime anidbid="17664" tvdbid="262954" defaulttvdbseason="5" episodeoffset="24" tmdbtv="45790" tmdbseason="5" tmdboffset="24"/>
 </anime-list>`
	snapshot, e := ParseAnimeLists([]byte(xml))
	if e != nil {
		t.Fatal(e)
	}
	r := NewResolver(nil, snapshot)
	for _, tc := range []struct {
		season int
		ids    string
	}{{1, "9304"}, {2, "10206,10835"}, {3, "11734"}, {4, "14173"}, {5, "16187,17286,17664"}} {
		got := r.Resolve(Query{Provider: "tvdb_show", ID: "262954", Season: tc.season})
		if got.Status != Resolved {
			t.Fatalf("season %d: %+v", tc.season, got)
		}
		ids := []string{}
		for _, m := range got.Matches {
			ids = append(ids, m.AniDBID)
			if !m.CoverageUnknown {
				t.Fatalf("offset inferred as exact: %+v", m)
			}
		}
		if strings.Join(ids, ",") != tc.ids {
			t.Fatalf("season %d: %v", tc.season, ids)
		}
	}
	for _, tc := range []struct {
		ep int
		id string
	}{{12, "16187"}, {13, "17286"}, {24, "17286"}, {25, "17664"}} {
		got := r.Resolve(Query{Provider: "tmdb_show", ID: "45790", Season: 5, Episodes: []int{tc.ep}})
		if got.Status != Resolved || len(got.Matches) != 1 || got.Matches[0].AniDBID != tc.id {
			t.Fatalf("episode %d: %+v", tc.ep, got)
		}
	}
	if got := r.Resolve(Query{Provider: "tvdb_show", ID: "262954", Season: 0}); got.Status != Missing {
		t.Fatalf("open offset suggested specials: %+v", got)
	}
}

func TestRejectBadSnapshot(t *testing.T) {
	for _, data := range []string{`{}`, `{"$meta":{"schema_version":"2.0"}}`, `{"$meta":{"schema_version":"3"},"bad":{"anidb:1:R":{}}}`} {
		if _, e := ParseAniBridge([]byte(data)); e == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestLiveAniBridgeSnapshot(t *testing.T) {
	if os.Getenv("LIVE_ANIME_MAPPINGS") != "1" {
		t.Skip("set LIVE_ANIME_MAPPINGS=1 for upstream snapshot check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, DefaultAniBridgeURL, nil)
	if e != nil {
		t.Fatal(e)
	}
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, MaxSnapshotBytes+1))
	if e != nil {
		t.Fatal(e)
	}
	snapshot, e := ParseAniBridge(data)
	if e != nil {
		t.Fatal(e)
	}
	got := NewResolver(snapshot, nil).Resolve(Query{Provider: "tvdb_show", ID: "262954", Season: 5})
	if got.Status != Resolved || len(got.Matches) != 3 {
		t.Fatalf("JoJo S5: %+v", got)
	}
	listReq, e := http.NewRequestWithContext(ctx, http.MethodGet, DefaultAnimeListsURL, nil)
	if e != nil {
		t.Fatal(e)
	}
	listResp, e := http.DefaultClient.Do(listReq)
	if e != nil {
		t.Fatal(e)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("Anime-Lists HTTP %d", listResp.StatusCode)
	}
	listData, e := io.ReadAll(io.LimitReader(listResp.Body, MaxSnapshotBytes+1))
	if e != nil {
		t.Fatal(e)
	}
	fallback, e := ParseAnimeLists(listData)
	if e != nil {
		t.Fatal(e)
	}
	got = NewResolver(nil, fallback).Resolve(Query{Provider: "tvdb_show", ID: "262954", Season: 5})
	if got.Status != Resolved || len(got.Matches) != 3 {
		t.Fatalf("Anime-Lists JoJo S5: %+v", got)
	}
}

func TestStoreKeepsLastValidSnapshot(t *testing.T) {
	good := []byte(`{"$meta":{"schema_version":"3"},"tvdb_show:12:s1":{"anidb:34:R":{"1-12":"1-12"}}}`)
	s := NewStore(nil)
	if _, e := s.LoadAniBridge(good); e != nil {
		t.Fatal(e)
	}
	if _, e := s.LoadAniBridge([]byte(`{"$meta":{"schema_version":"2"}}`)); e == nil {
		t.Fatal("accepted wrong major")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`broken`)) }))
	defer server.Close()
	s.HTTP = server.Client()
	if _, _, e := s.UpdateAniBridge(context.Background(), server.URL); e == nil {
		t.Fatal("accepted bad refresh")
	}
	got := s.Resolver().Resolve(Query{Provider: "tvdb_show", ID: "12", Season: 1})
	if got.Status != Resolved || got.Matches[0].AniDBID != "34" {
		t.Fatalf("lost last good: %+v", got)
	}
}
