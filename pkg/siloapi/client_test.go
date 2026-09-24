package siloapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGETRetries429And503WithoutChangingRequest(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/libraries" || r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("X-Profile-Id") != "primary" {
			t.Errorf("retry changed native request: %+v", r)
		}
		if requests <= 2 {
			w.Header().Set("Retry-After", "0")
			if requests == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		fmt.Fprint(w, `{"items":[{"id":"lib"}]}`)
	}))
	defer s.Close()
	c, err := New(s.URL, "key", "primary")
	if err != nil {
		t.Fatal(err)
	}
	libs, err := c.Libraries(context.Background())
	if err != nil || requests != 3 || len(libs) != 1 || libs[0].ID != "lib" {
		t.Fatalf("retry: requests=%d libs=%+v err=%v", requests, libs, err)
	}
}

func TestGETRetryExhaustionAndCancellation(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusNotFound} {
		requests := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(status)
		}))
		c, _ := New(s.URL, "key", "primary")
		_, err := c.Libraries(context.Background())
		want := 3
		if status == http.StatusNotFound {
			want = 1
		}
		if requests != want || err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
			t.Fatalf("status=%d attempts=%d err=%v", status, requests, err)
		}
		s.Close()
	}
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer s.Close()
	c, _ := New(s.URL, "key", "primary")
	started := time.Now()
	_, err := c.Libraries(context.Background())
	if requests.Load() != 1 || err == nil || !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "retry later") || time.Since(started) > time.Second {
		t.Fatalf("long Retry-After retried or waited: requests=%d err=%v", requests.Load(), err)
	}
}

func TestGETRetryWaitIsCancelable(t *testing.T) {
	var requests atomic.Int32
	first := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(first)
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer s.Close()
	c, _ := New(s.URL, "key", "primary")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-first; time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := c.Libraries(ctx)
	if !errors.Is(err, context.Canceled) || requests.Load() != 1 {
		t.Fatalf("cancel: requests=%d err=%v", requests.Load(), err)
	}
}

func TestGETHonorsShortRetryAfter(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":"lib"}]}`)
	}))
	defer s.Close()
	c, _ := New(s.URL, "key", "primary")
	started := time.Now()
	libs, err := c.Libraries(context.Background())
	if err != nil || requests.Load() != 2 || len(libs) != 1 || time.Since(started) < 900*time.Millisecond {
		t.Fatalf("short Retry-After was not honored: requests=%d elapsed=%v err=%v", requests.Load(), time.Since(started), err)
	}
}

func TestRetryAfterDelayPreservesRequestedWait(t *testing.T) {
	if got := siloRetryDelay("5", 100*time.Millisecond); got != 5*time.Second {
		t.Fatalf("delta=%v", got)
	}
	if got := siloRetryDelay("999999999999999999999999", 100*time.Millisecond); got <= 10*time.Second {
		t.Fatalf("overflowed delta retried early=%v", got)
	}
	if got := siloRetryDelay(time.Now().Add(24*time.Hour).UTC().Format(http.TimeFormat), 100*time.Millisecond); got <= 10*time.Second {
		t.Fatalf("date was shortened=%v", got)
	}
	if got := siloRetryDelay("invalid", 100*time.Millisecond); got != 100*time.Millisecond {
		t.Fatalf("fallback=%v", got)
	}
}

func TestInventoryUsesCatalogMembershipAndEveryFilePage(t *testing.T) {
	pagesRead := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("X-Profile-Id") != "primary" {
			t.Error("missing explicit HTTP credentials")
		}
		switch r.URL.Path {
		case "/api/v2/catalog":
			fmt.Fprint(w, `{"items":[{"content_id":"series:a","type":"series"}]}`)
		case "/api/v2/catalog/items/series:a":
			fmt.Fprint(w, `{"content_id":"series:a","type":"series","title":"A","tmdb_id":"10"}`)
		case "/api/v2/catalog/series/series:a/seasons":
			fmt.Fprint(w, `{"items":[{"content_id":"season:a:2","season_number":2}]}`)
		case "/api/v2/catalog/series/series:a/seasons/2/episodes":
			fmt.Fprint(w, `{"items":[{"content_id":"episode:a:1","season_number":2,"episode_number":1,"files":[{"file_id":"1"},{"file_id":"2"}]}]}`)
		case "/api/v2/admin/items/series:a/files":
			pagesRead++
			if r.URL.Query().Get("cursor") == "" {
				fmt.Fprint(w, `{"items":[{"id":"1","library_id":"1","file_path":"/media/A/Season 02/1.mkv","observed_root_path":"/media/A","season_number":99}],"page":{"has_more":true,"next_cursor":"two"}}`)
			} else {
				fmt.Fprint(w, `{"items":[{"id":"2","library_id":"1","file_path":"/media/A/Season 02/2.mkv","observed_root_path":"/media/A","season_number":99}],"page":{"has_more":false}}`)
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c, e := New(s.URL, "key", "primary")
	if e != nil {
		t.Fatal(e)
	}
	inv, e := c.Inventory(context.Background(), Library{ID: "1", Type: "series", Paths: []string{"/media"}})
	if e != nil {
		t.Fatal(e)
	}
	if pagesRead != 2 || len(inv.Files) != 2 || inv.Files[0].Season != 2 || inv.Files[0].SeasonID != "season:a:2" {
		t.Fatalf("wrong joined inventory: %+v", inv)
	}
}
func TestRejectUnexpectedBoundedPaginationAndCredentialRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, "https://example.invalid", 302)
					return
				}
				fmt.Fprint(w, `{"items":[{"id":"1"}],"page":{"has_more":true,"next_cursor":"same"}}`)
			}))
			defer s.Close()
			c, _ := New(s.URL, "secret", "p")
			if _, e := c.Libraries(context.Background()); e == nil {
				t.Fatal("accepted unsafe/incomplete response")
			}
		})
	}
}
func TestRejectRepeatedCursorOnPaginatedRoute(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("limit") != "100" {
			t.Errorf("missing paginated collection limit: %s", r.URL)
		}
		fmt.Fprint(w, `{"items":[{"id":"one"}],"page":{"has_more":true,"next_cursor":"same"}}`)
	}))
	defer server.Close()
	client, err := New(server.URL, "key", "profile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pages[Library](context.Background(), client, "/catalog", nil, true); err == nil || requests != 2 {
		t.Fatalf("repeated cursor was not refused after two pages: requests=%d err=%v", requests, err)
	}
}

func TestInventoryRefusesCatalogFileMissingFromSelectedLibrary(t *testing.T) {
	for _, tc := range []struct {
		name, kind, admin string
		refuse            bool
	}{
		{"series missing page row", "series", "one", true},
		{"series ID exists only in another library", "series", "foreign ID", true},
		{"series ignores unrelated foreign row", "series", "complete plus foreign", false},
		{"movie missing version", "movie", "one", true},
		{"movie complete versions", "movie", "complete", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/v2/catalog/") && r.URL.Query().Get("library_id") != "selected" && !strings.Contains(r.URL.Path, "/admin/") {
					t.Errorf("catalog request was not scoped to selected library: %s", r.URL)
				}
				switch r.URL.Path {
				case "/api/v2/catalog":
					fmt.Fprintf(w, `{"items":[{"content_id":"item","type":%q}]}`, tc.kind)
				case "/api/v2/catalog/items/item":
					fmt.Fprintf(w, `{"content_id":"item","type":%q}`, tc.kind)
				case "/api/v2/catalog/series/item/seasons":
					fmt.Fprint(w, `{"items":[{"content_id":"season","season_number":1}]}`)
				case "/api/v2/catalog/series/item/seasons/1/episodes":
					fmt.Fprint(w, `{"items":[{"season_number":1,"episode_number":1,"files":[{"file_id":"one"}]},{"season_number":1,"episode_number":2,"files":[{"file_id":"two"}]}]}`)
				case "/api/v2/catalog/items/item/versions":
					fmt.Fprint(w, `{"items":[{"file_id":"one"},{"file_id":"two"}]}`)
				case "/api/v2/admin/items/item/files":
					first := `{"id":"one","library_id":"selected","file_path":"/media/Show/one.mkv"}`
					switch tc.admin {
					case "one":
						fmt.Fprintf(w, `{"items":[%s],"page":{"has_more":false}}`, first)
					case "foreign ID":
						fmt.Fprintf(w, `{"items":[%s,{"id":"two","library_id":"other","file_path":"/other/two.mkv"}],"page":{"has_more":false}}`, first)
					case "complete plus foreign":
						fmt.Fprintf(w, `{"items":[%s,{"id":"two","library_id":"selected","file_path":"/media/Show/two.mkv"},{"id":"foreign","library_id":"other","file_path":"/other/foreign.mkv"}],"page":{"has_more":false}}`, first)
					case "complete":
						fmt.Fprintf(w, `{"items":[%s,{"id":"two","library_id":"selected","file_path":"/media/Show/two.mkv"}],"page":{"has_more":false}}`, first)
					}
				default:
					t.Errorf("unexpected route %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := New(server.URL, "key", "profile")
			if err != nil {
				t.Fatal(err)
			}
			inv, err := client.Inventory(context.Background(), Library{ID: "selected", Type: tc.kind})
			if err != nil {
				t.Fatal(err)
			}
			if got := inv.Refusals["item"] != ""; got != tc.refuse {
				t.Fatalf("refusal=%q, want refusal=%v", inv.Refusals["item"], tc.refuse)
			}
			if !tc.refuse && len(inv.Files) != 2 {
				t.Fatalf("selected-library files = %d, want 2", len(inv.Files))
			}
		})
	}
}

func TestCollectionQueryContractRejectsUnknownParameters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		want := url.Values{}
		body := ""
		switch r.URL.Path {
		case "/api/v2/libraries":
			body = `{"items":[{"id":"series","type":"series"},{"id":"movie","type":"movie"}]}`
		case "/api/v2/catalog":
			lib := q.Get("library_id")
			if lib != "series" && lib != "movie" {
				http.Error(w, "unknown library", http.StatusUnprocessableEntity)
				return
			}
			want = url.Values{"library_id": {lib}, "sort": {"title"}, "limit": {"100"}}
			body = fmt.Sprintf(`{"items":[{"content_id":%q,"type":%q}],"page":{"has_more":false}}`, lib+"-item", lib)
		case "/api/v2/catalog/items/series-item":
			want = url.Values{"library_id": {"series"}}
			body = `{"content_id":"series-item","type":"series"}`
		case "/api/v2/catalog/items/movie-item":
			want = url.Values{"library_id": {"movie"}}
			body = `{"content_id":"movie-item","type":"movie"}`
		case "/api/v2/catalog/series/series-item/seasons":
			want = url.Values{"library_id": {"series"}}
			body = `{"items":[{"content_id":"season-1","season_number":1}]}`
		case "/api/v2/catalog/series/series-item/seasons/1/episodes":
			want = url.Values{"library_id": {"series"}}
			body = `{"items":[{"season_number":1,"episode_number":1,"files":[{"file_id":"episode-file"}]}]}`
		case "/api/v2/catalog/items/movie-item/versions":
			want = url.Values{"library_id": {"movie"}}
			body = `{"items":[{"file_id":"movie-file"}]}`
		case "/api/v2/admin/items/series-item/files":
			want = url.Values{"limit": {"100"}}
			body = `{"items":[{"id":"episode-file","library_id":"series","file_path":"/media/Show/ep.mkv"}],"page":{"has_more":false}}`
		case "/api/v2/admin/items/movie-item/files":
			want = url.Values{"limit": {"100"}}
			body = `{"items":[{"id":"movie-file","library_id":"movie","file_path":"/media/Movie/movie.mkv"}],"page":{"has_more":false}}`
		case "/api/v2/admin/autoscan/settings":
			body = `{"enabled":true}`
		case "/api/v2/admin/autoscan/sources":
			want = url.Values{"limit": {"100"}}
			body = `{"items":[{"id":"source","plugin_id":"plugin","capability_id":"themes","enabled":true,"delivery_mode":"poll"}],"page":{"has_more":false}}`
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if !reflect.DeepEqual(q, want) {
			http.Error(w, fmt.Sprintf("unknown query parameters: got %v, want %v", q, want), http.StatusUnprocessableEntity)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	client, err := New(server.URL, "key", "profile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Libraries(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"series", "movie"} {
		inv, err := client.Inventory(context.Background(), Library{ID: kind, Type: kind})
		if err != nil || len(inv.Files) != 1 || inv.Refusals[kind+"-item"] != "" {
			t.Fatalf("%s inventory: %+v, %v", kind, inv, err)
		}
	}
	if _, err := client.ScanSource(context.Background(), "plugin"); err != nil {
		t.Fatal(err)
	}
}
