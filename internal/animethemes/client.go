// Package animethemes fetches exact AniDB matches and selects audio metadata.
// It never downloads or writes media.
package animethemes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/ratelimit"
)

const DefaultBaseURL = "https://api.animethemes.moe"

type Resource struct {
	ExternalID int    `json:"external_id"`
	Site       string `json:"site"`
}
type Audio struct {
	ID   int    `json:"id"`
	Link string `json:"link"`
	// Basename is the file name with its extension. The API's filename
	// once included the extension too, and now omits it.
	Basename string `json:"basename"`
	Filename string `json:"filename"`
	MIMEType string `json:"mimetype"`
	Size     int64  `json:"size"`
}
type Video struct {
	ID      int    `json:"id"`
	Overlap string `json:"overlap"`
	NC      bool   `json:"nc"`
	Audio   *Audio `json:"audio"`
}
type Entry struct {
	ID      int     `json:"id"`
	Version *int    `json:"version"`
	NSFW    bool    `json:"nsfw"`
	Spoiler bool    `json:"spoiler"`
	Videos  []Video `json:"videos"`
}
type Theme struct {
	ID       int     `json:"id"`
	Type     string  `json:"type"`
	Sequence *int    `json:"sequence"`
	Slug     string  `json:"slug"`
	Entries  []Entry `json:"animethemeentries"`
}
type Anime struct {
	ID        int        `json:"id"`
	Name      string     `json:"name"`
	Resources []Resource `json:"resources"`
	Themes    []Theme    `json:"animethemes"`
}
type Candidate struct {
	AnimeID, AniDBID, ThemeID, EntryID, VideoID, AudioID int
	Type                                                 string
	Sequence                                             int
	Title, URL, Filename, Extension, MIMEType            string
	Size                                                 int64
}
type Omission struct {
	ThemeID, EntryID int
	Reason           string
}
type Selection struct {
	Candidates []Candidate
	Omissions  []Omission
}
type Options struct{ OP, ED, AllDistinct, AllowSpoiler, AllowNSFW, AllowOverlap bool }

type cacheEntry struct {
	anime  []Anime
	expiry time.Time
}
type Client struct {
	HTTP                     *http.Client
	BaseURL                  string
	PositiveTTL, NegativeTTL time.Duration
	MaxPages                 int
	mu                       sync.Mutex
	cache                    map[int]cacheEntry
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{HTTP: httpClient, BaseURL: DefaultBaseURL, PositiveTTL: 24 * time.Hour, NegativeTTL: 2 * time.Hour, MaxPages: 20, cache: make(map[int]cacheEntry)}
}
func validID(id int) bool         { return id > 0 }
func copyAnime(a []Anime) []Anime { b := make([]Anime, len(a)); copy(b, a); return b }
func (c *Client) request(ctx context.Context, u string) (struct {
	Anime []Anime `json:"anime"`
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
}, error) {
	var page struct {
		Anime []Anime `json:"anime"`
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	}
	for attempt := 0; attempt < 3; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, e := http.NewRequestWithContext(requestCtx, http.MethodGet, u, nil)
		if e != nil {
			cancel()
			return page, e
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", ratelimit.UserAgent)
		client := c.HTTP
		if client == nil {
			client = &http.Client{Timeout: 15 * time.Second}
		}
		copyClient := *client
		copyClient.Transport = ratelimit.Wrap(client.Transport, ratelimit.ExternalInterval)
		copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, e := copyClient.Do(req)
		if e != nil {
			cancel()
			if errors.Is(e, ratelimit.ErrDeferred) {
				return page, errors.New("AnimeThemes rate limit exceeds request budget; retry later")
			}
			return page, e
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			cancel()
			if attempt == 2 {
				return page, fmt.Errorf("AnimeThemes HTTP %d; retry later", resp.StatusCode)
			}
			budgetCtx, budgetCancel := context.WithTimeout(ctx, 15*time.Second)
			deferred := ratelimit.Waiting(budgetCtx, req.URL)
			budgetCancel()
			if deferred {
				return page, fmt.Errorf("AnimeThemes HTTP %d; retry later", resp.StatusCode)
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			return page, fmt.Errorf("AnimeThemes HTTP %d", resp.StatusCode)
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
		resp.Body.Close()
		cancel()
		if e != nil {
			return page, e
		}
		if len(data) > 8<<20 {
			return page, errors.New("AnimeThemes response too large")
		}
		if e = json.Unmarshal(data, &page); e != nil {
			return page, e
		}
		if page.Anime == nil {
			return page, errors.New("AnimeThemes response is missing anime collection")
		}
		return page, nil
	}
	return page, errors.New("AnimeThemes retry limit reached")
}
func (c *Client) lookupBatch(ctx context.Context, ids []int) (map[int][]Anime, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	root, e := url.Parse(base)
	if e != nil || root.Scheme != "https" && !(root.Scheme == "http" && (root.Hostname() == "127.0.0.1" || root.Hostname() == "localhost")) {
		return nil, errors.New("AnimeThemes base URL must use HTTPS")
	}
	root.Path = strings.TrimRight(root.Path, "/") + "/anime/"
	joined := make([]string, len(ids))
	wanted := map[int]bool{}
	for i, id := range ids {
		joined[i] = strconv.Itoa(id)
		wanted[id] = true
	}
	q := root.Query()
	q.Set("filter[resource][external_id]", strings.Join(joined, ","))
	q.Set("filter[resource][site]", "aniDB")
	q.Set("filter[has]", "resources")
	q.Set("include", "animethemes.animethemeentries.videos.audio,resources")
	q.Set("page[size]", "100")
	root.RawQuery = q.Encode()
	out := make(map[int][]Anime, len(ids))
	seen := map[string]bool{}
	maxPages := c.MaxPages
	if maxPages <= 0 {
		maxPages = 20
	}
	for pageNo, u := 1, root.String(); u != ""; pageNo++ {
		if pageNo > maxPages {
			return nil, errors.New("AnimeThemes pagination limit reached")
		}
		if seen[u] {
			return nil, errors.New("AnimeThemes pagination cycle")
		}
		seen[u] = true
		page, e := c.request(ctx, u)
		if e != nil {
			return nil, e
		}
		for _, a := range page.Anime {
			for _, r := range a.Resources {
				if strings.EqualFold(r.Site, "AniDB") && wanted[r.ExternalID] {
					out[r.ExternalID] = append(out[r.ExternalID], a)
				}
			}
		}
		if page.Links.Next == "" {
			break
		}
		next, e := url.Parse(page.Links.Next)
		if e != nil {
			return nil, e
		}
		resolved := root.ResolveReference(next)
		if resolved.Scheme != root.Scheme || resolved.Host != root.Host || resolved.Path != root.Path {
			return nil, errors.New("AnimeThemes unsafe next-page URL")
		}
		u = resolved.String()
	}
	for id, list := range out {
		slices.SortFunc(list, func(a, b Anime) int { return a.ID - b.ID })
		out[id] = list
	}
	return out, nil
}

// LookupAniDB batches up to 100 IDs per request, follows bounded pagination,
// and verifies each returned anime contains the requested AniDB resource.
func (c *Client) LookupAniDB(ctx context.Context, ids []int) (map[int][]Anime, error) {
	if c == nil {
		return nil, errors.New("nil AnimeThemes client")
	}
	uniq := map[int]bool{}
	for _, id := range ids {
		if !validID(id) {
			return nil, fmt.Errorf("invalid AniDB ID %d", id)
		}
		uniq[id] = true
	}
	keys := make([]int, 0, len(uniq))
	for id := range uniq {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	out := make(map[int][]Anime, len(keys))
	missing := []int{}
	now := time.Now()
	c.mu.Lock()
	if c.cache == nil {
		c.cache = make(map[int]cacheEntry)
	}
	for _, id := range keys {
		if hit, ok := c.cache[id]; ok && now.Before(hit.expiry) {
			out[id] = copyAnime(hit.anime)
		} else {
			missing = append(missing, id)
		}
	}
	c.mu.Unlock()
	for start := 0; start < len(missing); start += 100 {
		end := min(start+100, len(missing))
		batch, e := c.lookupBatch(ctx, missing[start:end])
		if e != nil {
			return nil, e
		}
		c.mu.Lock()
		for _, id := range missing[start:end] {
			list := copyAnime(batch[id])
			out[id] = list
			ttl := c.PositiveTTL
			if len(list) == 0 {
				ttl = c.NegativeTTL
			}
			if ttl <= 0 {
				ttl = time.Hour
			}
			c.cache[id] = cacheEntry{list, time.Now().Add(ttl)}
		}
		c.mu.Unlock()
	}
	return out, nil
}

func audioExt(a Audio) string {
	u, e := url.Parse(a.Link)
	if e != nil || u.Scheme != "https" || u.Host == "" || a.ID <= 0 || a.Size <= 0 {
		return ""
	}
	name := a.Basename
	if name == "" {
		name = a.Filename
	}
	lower := strings.ToLower(path.Ext(u.EscapedPath()))
	for _, ext := range []string{".mp3", ".m4a", ".m4b", ".flac", ".ogg", ".opus", ".wav", ".aac"} {
		if lower == ext && strings.EqualFold(path.Ext(name), ext) {
			return strings.TrimPrefix(ext, ".")
		}
	}
	return ""
}
func rank(v Video) int {
	n := 0
	if v.NC {
		n -= 2
	}
	if strings.EqualFold(v.Overlap, "None") {
		n -= 1
	}
	return n
}
func aniDBID(a Anime) int {
	for _, r := range a.Resources {
		if strings.EqualFold(r.Site, "AniDB") {
			return r.ExternalID
		}
	}
	return 0
}

// Select returns at most one safe audio variant per theme unless AllDistinct
// is enabled. Identity is theme ID plus audio ID, independent of list order.
func Select(anime []Anime, opts Options) Selection {
	var out Selection
	seen := map[string]bool{}
	for _, a := range anime {
		if a.ID <= 0 || aniDBID(a) <= 0 {
			continue
		}
		for _, t := range a.Themes {
			if t.ID <= 0 || t.Type != "OP" && t.Type != "ED" {
				continue
			}
			if t.Type == "OP" && !opts.OP || t.Type == "ED" && !opts.ED {
				continue
			}
			seq := 0
			if t.Sequence != nil {
				seq = *t.Sequence
			}
			choices := []struct {
				Candidate
				rank int
			}{}
			for _, entry := range t.Entries {
				if entry.ID <= 0 {
					out.Omissions = append(out.Omissions, Omission{t.ID, entry.ID, "missing_identity"})
					continue
				}
				reason := ""
				if entry.NSFW && !opts.AllowNSFW {
					reason = "nsfw"
				}
				if entry.Spoiler && !opts.AllowSpoiler {
					reason = "spoiler"
				}
				if reason != "" {
					out.Omissions = append(out.Omissions, Omission{t.ID, entry.ID, reason})
					continue
				}
				for _, v := range entry.Videos {
					if v.ID <= 0 {
						out.Omissions = append(out.Omissions, Omission{t.ID, entry.ID, "missing_identity"})
						continue
					}
					if !opts.AllowOverlap && !strings.EqualFold(v.Overlap, "None") {
						out.Omissions = append(out.Omissions, Omission{t.ID, entry.ID, "overlap"})
						continue
					}
					if v.Audio == nil || audioExt(*v.Audio) == "" {
						out.Omissions = append(out.Omissions, Omission{t.ID, entry.ID, "missing_or_invalid_audio"})
						continue
					}
					choices = append(choices, struct {
						Candidate
						rank int
					}{Candidate{AnimeID: a.ID, AniDBID: aniDBID(a), ThemeID: t.ID, EntryID: entry.ID, VideoID: v.ID, AudioID: v.Audio.ID, Type: t.Type, Sequence: seq, Title: t.Slug, URL: v.Audio.Link, Filename: v.Audio.Filename, Extension: audioExt(*v.Audio), MIMEType: v.Audio.MIMEType, Size: v.Audio.Size}, rank(v)})
				}
			}
			slices.SortFunc(choices, func(x, y struct {
				Candidate
				rank int
			}) int {
				if x.rank != y.rank {
					return x.rank - y.rank
				}
				if x.EntryID != y.EntryID {
					return x.EntryID - y.EntryID
				}
				if x.AudioID != y.AudioID {
					return x.AudioID - y.AudioID
				}
				return x.VideoID - y.VideoID
			})
			for _, ch := range choices {
				key := fmt.Sprintf("%d:%d", t.ID, ch.AudioID)
				if seen[key] {
					continue
				}
				seen[key] = true
				out.Candidates = append(out.Candidates, ch.Candidate)
				if !opts.AllDistinct {
					break
				}
			}
		}
	}
	slices.SortFunc(out.Candidates, func(a, b Candidate) int {
		if a.Type != b.Type {
			return strings.Compare(a.Type, b.Type)
		}
		if a.Sequence != b.Sequence {
			return a.Sequence - b.Sequence
		}
		if a.ThemeID != b.ThemeID {
			return a.ThemeID - b.ThemeID
		}
		return a.AudioID - b.AudioID
	})
	slices.SortFunc(out.Omissions, func(a, b Omission) int {
		if a.ThemeID != b.ThemeID {
			return a.ThemeID - b.ThemeID
		}
		if a.EntryID != b.EntryID {
			return a.EntryID - b.EntryID
		}
		return strings.Compare(a.Reason, b.Reason)
	})
	return out
}
