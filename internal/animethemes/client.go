// Package animethemes fetches exact AniDB matches and selects audio metadata.
// It never downloads or writes media.
package animethemes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/ratelimit"
)

// DefaultBaseURL is the AnimeThemes GraphQL endpoint. The JSON:API at
// api.animethemes.moe has answered 503 since 2026-11-10.
const DefaultBaseURL = "https://graphql.animethemes.moe/"

// batchSize is the most AniDB IDs findAnimeByExternalSite accepts at once.
const batchSize = 100

// defaultPageSize bounds each nested list in one query. A list that fills it
// is fetched again page by page.
const defaultPageSize = 1000

type Resource struct {
	ExternalID int
	Site       string
}
type Audio struct {
	ID   int
	Link string
	// Basename is the file name with its extension; Filename omits it.
	Basename string
	Filename string
	MIMEType string
	Size     int64
}
type Video struct {
	ID      int
	Overlap string // NONE, TRANS or OVER
	NC      bool
	Audio   *Audio
}
type Entry struct {
	ID      int
	Version *int
	NSFW    bool
	Spoiler bool
	Videos  []Video
}
type Theme struct {
	ID       int
	Type     string // OP, ED or IN
	Sequence *int
	Slug     string
	Entries  []Entry
}
type Anime struct {
	ID        int
	Name      string
	Resources []Resource
	Themes    []Theme
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
	HTTP *http.Client
	// BaseURL is the GraphQL endpoint.
	BaseURL                  string
	PositiveTTL, NegativeTTL time.Duration
	// MaxPages bounds the follow-up requests for nested lists in one batch.
	MaxPages int
	// PageSize is the length of each nested list page; zero means 1000.
	PageSize int
	mu       sync.Mutex
	cache    map[int]cacheEntry
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{HTTP: httpClient, BaseURL: DefaultBaseURL, PositiveTTL: 24 * time.Hour, NegativeTTL: 2 * time.Hour, MaxPages: 20, cache: make(map[int]cacheEntry)}
}
func validID(id int) bool         { return id > 0 }
func copyAnime(a []Anime) []Anime { b := make([]Anime, len(a)); copy(b, a); return b }

// GraphQL rejects unused fragments, so each query lists only the ones it uses.
const (
	videoFragment = `fragment V on Video { id overlap nc audio { id link basename filename mimetype size } }
`
	entryFragment = `fragment E on AnimeThemeEntry { id version nsfw spoiler videos(first: $n) { pageInfo { hasNextPage endCursor } nodes { ...V } } }
`
	themeFragment = `fragment T on AnimeTheme { id type sequence slug animethemeentries(first: $n) { ...E } }
`
	lookupQuery = videoFragment + entryFragment + themeFragment +
		`query Lookup($ids: [Int!], $n: Int!) { findAnimeByExternalSite(site: ANIDB, id: $ids) { id title { romaji } resources(site: ANIDB, first: $n) { pageInfo { hasNextPage } nodes { site externalId } } animethemes(first: $n) { ...T } } }`
	themesQuery = videoFragment + entryFragment + themeFragment +
		`query Themes($ids: [Int!], $n: Int!, $page: Int) { findAnimeByExternalSite(site: ANIDB, id: $ids) { id animethemes(first: $n, page: $page) { ...T } } }`
	entriesQuery = videoFragment + entryFragment +
		`query Entries($theme: Int!, $n: Int!, $page: Int) { animethemePagination(id: $theme, first: 1) { data { id animethemeentries(first: $n, page: $page) { ...E } } } }`
	videosQuery = videoFragment +
		`query Videos($entry: Mixed, $n: Int!, $after: String) { animethemeentryPagination(where: {column: ID, operator: EQ, value: $entry}, first: 1) { data { id videos(first: $n, after: $after) { pageInfo { hasNextPage endCursor } nodes { ...V } } } } }`
)

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}
type gqlAudio struct {
	ID       int    `json:"id"`
	Link     string `json:"link"`
	Basename string `json:"basename"`
	Filename string `json:"filename"`
	MIMEType string `json:"mimetype"`
	Size     int64  `json:"size"`
}
type gqlVideo struct {
	ID      int       `json:"id"`
	Overlap string    `json:"overlap"`
	NC      bool      `json:"nc"`
	Audio   *gqlAudio `json:"audio"`
}
type gqlVideos struct {
	PageInfo pageInfo   `json:"pageInfo"`
	Nodes    []gqlVideo `json:"nodes"`
}
type gqlEntry struct {
	ID      int       `json:"id"`
	Version *int      `json:"version"`
	NSFW    bool      `json:"nsfw"`
	Spoiler bool      `json:"spoiler"`
	Videos  gqlVideos `json:"videos"`
}
type gqlTheme struct {
	ID       int        `json:"id"`
	Type     string     `json:"type"`
	Sequence *int       `json:"sequence"`
	Slug     string     `json:"slug"`
	Entries  []gqlEntry `json:"animethemeentries"`
}
type gqlAnime struct {
	ID    int `json:"id"`
	Title struct {
		Romaji string `json:"romaji"`
	} `json:"title"`
	Resources struct {
		PageInfo pageInfo `json:"pageInfo"`
		Nodes    []struct {
			Site       string `json:"site"`
			ExternalID *int   `json:"externalId"`
		} `json:"nodes"`
	} `json:"resources"`
	Themes []gqlTheme `json:"animethemes"`
}

func (g gqlAnime) convert() Anime {
	a := Anime{ID: g.ID, Name: g.Title.Romaji}
	for _, r := range g.Resources.Nodes {
		if r.ExternalID != nil {
			a.Resources = append(a.Resources, Resource{ExternalID: *r.ExternalID, Site: r.Site})
		}
	}
	for _, t := range g.Themes {
		theme := Theme{ID: t.ID, Type: t.Type, Sequence: t.Sequence, Slug: t.Slug}
		for _, e := range t.Entries {
			entry := Entry{ID: e.ID, Version: e.Version, NSFW: e.NSFW, Spoiler: e.Spoiler}
			for _, v := range e.Videos.Nodes {
				video := Video{ID: v.ID, Overlap: v.Overlap, NC: v.NC}
				if v.Audio != nil {
					video.Audio = &Audio{ID: v.Audio.ID, Link: v.Audio.Link, Basename: v.Audio.Basename, Filename: v.Audio.Filename, MIMEType: v.Audio.MIMEType, Size: v.Audio.Size}
				}
				entry.Videos = append(entry.Videos, video)
			}
			theme.Entries = append(theme.Entries, entry)
		}
		a.Themes = append(a.Themes, theme)
	}
	return a
}

func (c *Client) endpoint() (*url.URL, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return nil, errors.New("AnimeThemes base URL must use HTTPS")
	}
	return u, nil
}

// request posts one GraphQL query and decodes its data into out. A response
// with GraphQL errors is refused whole, so partial data is never cached.
func (c *Client) request(ctx context.Context, endpoint *url.URL, query string, variables map[string]any, out any) error {
	body, e := json.Marshal(map[string]any{"query": query, "variables": variables})
	if e != nil {
		return e
	}
	for attempt := 0; attempt < 3; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, e := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if e != nil {
			cancel()
			return e
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
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
				return errors.New("AnimeThemes rate limit exceeds request budget; retry later")
			}
			return e
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			cancel()
			if attempt == 2 {
				return fmt.Errorf("AnimeThemes HTTP %d; retry later", resp.StatusCode)
			}
			budgetCtx, budgetCancel := context.WithTimeout(ctx, 15*time.Second)
			deferred := ratelimit.Waiting(budgetCtx, req.URL)
			budgetCancel()
			if deferred {
				return fmt.Errorf("AnimeThemes HTTP %d; retry later", resp.StatusCode)
			}
			continue
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		resp.Body.Close()
		cancel()
		if e != nil {
			return e
		}
		if len(data) > maxResponse {
			return errors.New("AnimeThemes response too large")
		}
		var envelope struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		decodeErr := json.Unmarshal(data, &envelope)
		if decodeErr == nil && len(envelope.Errors) > 0 {
			msg := envelope.Errors[0].Message
			if len(msg) > 200 {
				msg = msg[:200]
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("AnimeThemes HTTP %d: %s", resp.StatusCode, msg)
			}
			return fmt.Errorf("AnimeThemes query refused: %s", msg)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("AnimeThemes HTTP %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return decodeErr
		}
		if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return errors.New("AnimeThemes response is missing data")
		}
		return json.Unmarshal(envelope.Data, out)
	}
	return errors.New("AnimeThemes retry limit reached")
}

const maxResponse = 16 << 20

// pager fetches the later pages of nested lists that filled their first page,
// within a request budget shared by one batch.
type pager struct {
	c        *Client
	ctx      context.Context
	endpoint *url.URL
	size     int
	budget   int
}

func (p *pager) query(query string, variables map[string]any, out any) error {
	if p.budget <= 0 {
		return errors.New("AnimeThemes pagination limit reached")
	}
	p.budget--
	variables["n"] = p.size
	return p.c.request(p.ctx, p.endpoint, query, variables, out)
}

// completeAnime fetches the remaining theme pages of an anime found through
// the given AniDB ID, then completes each theme.
func (p *pager) completeAnime(a *gqlAnime, aniDB int) error {
	if a.Resources.PageInfo.HasNextPage {
		return errors.New("AnimeThemes returned too many AniDB resources for one anime")
	}
	for page, last := 2, len(a.Themes); last >= p.size; page++ {
		var data struct {
			Find []gqlAnime `json:"findAnimeByExternalSite"`
		}
		if e := p.query(themesQuery, map[string]any{"ids": []int{aniDB}, "page": page}, &data); e != nil {
			return e
		}
		last = 0
		for _, g := range data.Find {
			if g.ID == a.ID {
				a.Themes = append(a.Themes, g.Themes...)
				last = len(g.Themes)
			}
		}
	}
	for i := range a.Themes {
		if e := p.completeTheme(&a.Themes[i]); e != nil {
			return e
		}
	}
	return nil
}

func (p *pager) completeTheme(t *gqlTheme) error {
	for page, last := 2, len(t.Entries); last >= p.size; page++ {
		var data struct {
			Pagination struct {
				Data []gqlTheme `json:"data"`
			} `json:"animethemePagination"`
		}
		if e := p.query(entriesQuery, map[string]any{"theme": t.ID, "page": page}, &data); e != nil {
			return e
		}
		last = 0
		for _, g := range data.Pagination.Data {
			if g.ID == t.ID {
				t.Entries = append(t.Entries, g.Entries...)
				last = len(g.Entries)
			}
		}
	}
	for i := range t.Entries {
		if e := p.completeVideos(&t.Entries[i]); e != nil {
			return e
		}
	}
	return nil
}

func (p *pager) completeVideos(entry *gqlEntry) error {
	seen := map[string]bool{}
	for info := entry.Videos.PageInfo; info.HasNextPage; {
		if info.EndCursor == "" || seen[info.EndCursor] {
			return errors.New("AnimeThemes pagination cycle")
		}
		seen[info.EndCursor] = true
		var data struct {
			Pagination struct {
				Data []gqlEntry `json:"data"`
			} `json:"animethemeentryPagination"`
		}
		if e := p.query(videosQuery, map[string]any{"entry": entry.ID, "after": info.EndCursor}, &data); e != nil {
			return e
		}
		info = pageInfo{}
		for _, g := range data.Pagination.Data {
			if g.ID == entry.ID {
				entry.Videos.Nodes = append(entry.Videos.Nodes, g.Videos.Nodes...)
				info = g.Videos.PageInfo
			}
		}
	}
	entry.Videos.PageInfo = pageInfo{}
	return nil
}

func (c *Client) lookupBatch(ctx context.Context, ids []int) (map[int][]Anime, error) {
	endpoint, e := c.endpoint()
	if e != nil {
		return nil, e
	}
	size := c.PageSize
	if size <= 0 {
		size = defaultPageSize
	}
	maxPages := c.MaxPages
	if maxPages <= 0 {
		maxPages = 20
	}
	wanted := map[int]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var data struct {
		Find *[]gqlAnime `json:"findAnimeByExternalSite"`
	}
	if e := c.request(ctx, endpoint, lookupQuery, map[string]any{"ids": ids, "n": size}, &data); e != nil {
		return nil, e
	}
	if data.Find == nil {
		return nil, errors.New("AnimeThemes response is missing anime collection")
	}
	p := &pager{c: c, ctx: ctx, endpoint: endpoint, size: size, budget: maxPages}
	out := make(map[int][]Anime, len(ids))
	for _, g := range *data.Find {
		matched := []int{}
		for _, r := range g.Resources.Nodes {
			if strings.EqualFold(r.Site, "ANIDB") && r.ExternalID != nil && wanted[*r.ExternalID] && !slices.Contains(matched, *r.ExternalID) {
				matched = append(matched, *r.ExternalID)
			}
		}
		if len(matched) == 0 {
			continue
		}
		if e := p.completeAnime(&g, matched[0]); e != nil {
			return nil, e
		}
		a := g.convert()
		for _, id := range matched {
			out[id] = append(out[id], a)
		}
	}
	for id, list := range out {
		slices.SortFunc(list, func(a, b Anime) int { return a.ID - b.ID })
		out[id] = list
	}
	return out, nil
}

// LookupAniDB batches up to 100 IDs per GraphQL query, the most AnimeThemes
// accepts, follows bounded pagination of nested lists, and verifies each
// returned anime contains the requested AniDB resource.
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
	for start := 0; start < len(missing); start += batchSize {
		end := min(start+batchSize, len(missing))
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
