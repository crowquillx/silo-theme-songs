// Package siloapi implements the native V2 catalog contract the plugin reads,
// plus the scan request it makes after publishing a theme.
package siloapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/ratelimit"
)

type Library struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Paths   []string `json:"paths"`
	Enabled bool     `json:"enabled"`
}
type File struct {
	ID           string `json:"id"`
	LibraryID    string `json:"library_id"`
	Path         string `json:"file_path"`
	ObservedRoot string `json:"observed_root_path"`
}
type Version struct {
	FileID string `json:"file_id"`
	Path   string `json:"file_path"`
}
type Themes struct {
	OwnerID string `json:"owner_id"`
	Items   []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"items"`
}
type Item struct {
	ID       string    `json:"content_id"`
	Type     string    `json:"type"`
	Title    string    `json:"title"`
	Year     int       `json:"year"`
	TMDB     string    `json:"tmdb_id"`
	TVDB     string    `json:"tvdb_id"`
	IMDB     string    `json:"imdb_id"`
	Versions []Version `json:"versions"`
	Themes   *Themes   `json:"themes"`
}
type Season struct {
	ID     string `json:"content_id"`
	Number int    `json:"season_number"`
	Title  string `json:"title"`
}
type Episode struct {
	ID     string    `json:"content_id"`
	Season int       `json:"season_number"`
	Number int       `json:"episode_number"`
	Files  []Version `json:"files"`
}
type Membership struct {
	File
	ItemID   string
	Kind     string
	Season   int
	Episode  int
	SeasonID string
}
type Inventory struct {
	Library  Library
	Items    []Item
	Files    []Membership
	Seasons  map[string][]Season
	Refusals map[string]string
	// Unmatched holds admin files of an item that are absent from the
	// catalog. Silo keeps rows for deleted files, such as releases replaced
	// by an upgrade, so the resolver checks whether each still exists.
	Unmatched map[string][]File
}
type Client struct {
	base    *url.URL
	key     string
	http    *http.Client
	mu      sync.Mutex
	profile string // empty until the primary profile is looked up
}

// New never follows redirects with the Silo credential. Plain HTTP is allowed
// for an operator-selected internal Silo endpoint, never for provider downloads.
func New(base, key, profile string) (*Client, error) {
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Silo base URL")
	}
	if key == "" {
		return nil, errors.New("Silo API key is required")
	}
	return &Client{base: u, key: key, profile: profile, http: &http.Client{Timeout: 30 * time.Second, Transport: ratelimit.Wrap(nil, ratelimit.SiloInterval), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// profileID returns the configured profile, or the API key owner's primary
// profile when none is configured.
func (c *Client) profileID(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profile != "" {
		return c.profile, nil
	}
	var v struct {
		Items []struct {
			ID      string `json:"id"`
			Primary bool   `json:"is_primary"`
		} `json:"items"`
	}
	if e := c.request(ctx, "/profiles", nil, "", &v); e != nil {
		return "", e
	}
	for _, p := range v.Items {
		if p.Primary && p.ID != "" {
			c.profile = p.ID
			return p.ID, nil
		}
	}
	return "", errors.New("the API key's account has no primary profile; set profile_id")
}
func (c *Client) get(ctx context.Context, route string, q url.Values, out any) error {
	profile, e := c.profileID(ctx)
	if e != nil {
		return e
	}
	return c.request(ctx, route, q, profile, out)
}
func (c *Client) url(route string, q url.Values) url.URL {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v2" + route
	u.RawQuery = q.Encode()
	return u
}

func (c *Client) headers(r *http.Request, profile string) {
	r.Header.Set("Authorization", "Bearer "+c.key)
	if profile != "" {
		r.Header.Set("X-Profile-Id", profile)
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", ratelimit.UserAgent)
}

func (c *Client) request(ctx context.Context, route string, q url.Values, profile string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := c.url(route, q)
	for attempt := 0; attempt < 3; attempt++ {
		r, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if e != nil {
			return errors.New("invalid Silo request")
		}
		c.headers(r, profile)
		resp, e := c.http.Do(r)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(e, ratelimit.ErrDeferred) {
				return errors.New("Silo read rate limit exceeds remaining request budget; retry later")
			}
			return errors.New("Silo request failed")
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			status := resp.StatusCode
			resp.Body.Close()
			if attempt == 2 {
				return fmt.Errorf("Silo read returned HTTP %d", status)
			}
			if ratelimit.Waiting(ctx, &u) {
				return fmt.Errorf("Silo read returned HTTP %d; Retry-After exceeds remaining request budget, retry later", status)
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("Silo read returned HTTP %d", resp.StatusCode)
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
		closeErr := resp.Body.Close()
		if e != nil || closeErr != nil {
			return errors.New("incomplete Silo response")
		}
		if len(b) > 16<<20 {
			return errors.New("Silo response exceeds 16 MiB")
		}
		if e = json.Unmarshal(b, out); e != nil {
			return errors.New("invalid Silo JSON response")
		}
		return nil
	}
	return errors.New("Silo read retries exhausted")
}

func pages[T any](ctx context.Context, c *Client, path string, q url.Values, paginated bool) ([]T, error) {
	if q == nil {
		q = url.Values{}
	}
	if paginated {
		q.Set("limit", "100")
	}
	seen := map[string]bool{}
	var result []T
	for page := 0; page < 10000; page++ {
		var data struct {
			Items []T `json:"items"`
			Page  *struct {
				Next string `json:"next_cursor"`
				More bool   `json:"has_more"`
			} `json:"page"`
		}
		if e := c.get(ctx, path, q, &data); e != nil {
			return nil, e
		}
		if data.Items == nil {
			return nil, errors.New("Silo collection is missing items")
		}
		result = append(result, data.Items...)
		if len(result) > 1000000 {
			return nil, errors.New("Silo inventory limit exceeded")
		}
		if data.Page == nil || (!data.Page.More && data.Page.Next == "") {
			return result, nil
		}
		if !paginated {
			return nil, errors.New("bounded Silo collection unexpectedly requires pagination")
		}
		if !data.Page.More || data.Page.Next == "" || seen[data.Page.Next] || len(data.Items) == 0 {
			return nil, errors.New("incomplete or repeated Silo pagination")
		}
		seen[data.Page.Next] = true
		q.Set("cursor", data.Page.Next)
	}
	return nil, errors.New("Silo pagination limit exceeded")
}
func (c *Client) Libraries(ctx context.Context) ([]Library, error) {
	return pages[Library](ctx, c, "/libraries", nil, false)
}

// ErrScanUnavailable means this Silo cannot take scan requests through the
// API, either because it predates them or because it has no scanner.
var ErrScanUnavailable = errors.New("Silo does not accept scan requests through its API")

// RequestScan asks Silo to scan one file or folder of a library. Silo answers
// before the scan runs. The request is not retried: Silo marks it
// non-retryable, and a later discovery check asks again.
func (c *Client) RequestScan(ctx context.Context, libraryID, path string) error {
	profile, e := c.profileID(ctx)
	if e != nil {
		return e
	}
	body, e := json.Marshal(map[string]string{"library_id": libraryID, "path": path})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := c.url("/scan", nil)
	r, e := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if e != nil {
		return errors.New("invalid Silo request")
	}
	c.headers(r, profile)
	r.Header.Set("Content-Type", "application/json")
	resp, e := c.http.Do(r)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Silo scan request failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented:
		return ErrScanUnavailable
	}
	return fmt.Errorf("Silo scan request returned HTTP %d", resp.StatusCode)
}

func Eligible(l Library) bool {
	switch l.Type {
	case "movie", "movies", "tv", "series", "mixed":
		return true
	}
	return false
}
func (c *Client) ThemeCapability(ctx context.Context) error {
	var v struct {
		State string `json:"state"`
	}
	if e := c.get(ctx, "/catalog/themes/capabilities", nil, &v); e != nil {
		return e
	}
	if v.State != "available" {
		return errors.New("theme audio capability unavailable")
	}
	return nil
}
func (c *Client) Detail(ctx context.Context, id, library string) (Item, error) {
	var v Item
	e := c.get(ctx, "/catalog/items/"+id, url.Values{"library_id": {library}}, &v)
	if e == nil && v.ID != id {
		e = errors.New("catalog item identity mismatch")
	}
	return v, e
}
func (c *Client) Inventory(ctx context.Context, lib Library) (Inventory, error) {
	return c.InventoryProgress(ctx, lib, nil)
}

// InventoryProgress reads a library and reports each item read.
func (c *Client) InventoryProgress(ctx context.Context, lib Library, progress func(done, total int)) (Inventory, error) {
	inv := newInventory(lib)
	if !Eligible(lib) {
		return inv, errors.New("unsupported library kind")
	}
	rows, e := pages[Item](ctx, c, "/catalog", url.Values{"library_id": {lib.ID}, "sort": {"title"}}, true)
	if e != nil {
		return inv, e
	}
	seen := map[string]bool{}
	filesSeen := map[string]bool{}
	for i, row := range rows {
		if progress != nil {
			progress(i, len(rows))
		}
		if row.Type != "movie" && row.Type != "series" {
			continue
		}
		if seen[row.ID] {
			return inv, errors.New("duplicate catalog item")
		}
		seen[row.ID] = true
		if e := c.addItem(ctx, &inv, row.ID, filesSeen); e != nil {
			return inv, e
		}
	}
	return inv, nil
}

// ItemIDs lists the movies and series in a library without reading them.
func (c *Client) ItemIDs(ctx context.Context, lib Library) ([]string, error) {
	rows, e := pages[Item](ctx, c, "/catalog", url.Values{"library_id": {lib.ID}, "sort": {"title"}}, true)
	if e != nil {
		return nil, e
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Type == "movie" || row.Type == "series" {
			ids = append(ids, row.ID)
		}
	}
	return ids, nil
}

// InventoryItems reads the listed items of a library and reports each item
// read. Unknown IDs fail the read.
func (c *Client) InventoryItems(ctx context.Context, lib Library, ids []string, progress func(done, total int)) (Inventory, error) {
	inv := newInventory(lib)
	if !Eligible(lib) {
		return inv, errors.New("unsupported library kind")
	}
	filesSeen := map[string]bool{}
	for i, id := range ids {
		if progress != nil {
			progress(i, len(ids))
		}
		if e := c.addItem(ctx, &inv, id, filesSeen); e != nil {
			return inv, e
		}
	}
	return inv, nil
}

// ItemInventory reads one item of a library. The resolver refuses unknown
// videos on disk, so files of other items in the same folder still block it.
func (c *Client) ItemInventory(ctx context.Context, lib Library, id string) (Inventory, error) {
	inv := newInventory(lib)
	if !Eligible(lib) {
		return inv, errors.New("unsupported library kind")
	}
	return inv, c.addItem(ctx, &inv, id, map[string]bool{})
}

func newInventory(lib Library) Inventory {
	return Inventory{Library: lib, Seasons: map[string][]Season{}, Refusals: map[string]string{}, Unmatched: map[string][]File{}}
}

func (c *Client) addItem(ctx context.Context, inv *Inventory, id string, filesSeen map[string]bool) error {
	lib := inv.Library
	item, e := c.Detail(ctx, id, lib.ID)
	if e != nil {
		return e
	}
	inv.Items = append(inv.Items, item)
	fs, e := pages[File](ctx, c, "/admin/items/"+item.ID+"/files", nil, true)
	if e != nil {
		return e
	}
	membership := map[string]Membership{}
	if item.Type == "series" {
		seasons, e := pages[Season](ctx, c, "/catalog/series/"+item.ID+"/seasons", url.Values{"library_id": {lib.ID}}, false)
		if e != nil {
			return e
		}
		inv.Seasons[item.ID] = seasons
		for _, s := range seasons {
			eps, e := pages[Episode](ctx, c, fmt.Sprintf("/catalog/series/%s/seasons/%d/episodes", item.ID, s.Number), url.Values{"library_id": {lib.ID}}, false)
			if e != nil {
				return e
			}
			for _, ep := range eps {
				if ep.Season != s.Number {
					return errors.New("episode season mismatch")
				}
				for _, f := range ep.Files {
					if old, ok := membership[f.FileID]; ok && (old.Season != ep.Season || old.Episode != ep.Number) {
						return errors.New("ambiguous episode file membership")
					}
					membership[f.FileID] = Membership{ItemID: item.ID, Kind: item.Type, Season: ep.Season, Episode: ep.Number, SeasonID: s.ID}
				}
			}
		}
	} else {
		// The dedicated versions route carries the complete bounded version set.
		versions, e := pages[Version](ctx, c, "/catalog/items/"+item.ID+"/versions", url.Values{"library_id": {lib.ID}}, false)
		if e != nil {
			return e
		}
		for _, f := range versions {
			membership[f.FileID] = Membership{ItemID: item.ID, Kind: item.Type, Season: -1}
		}
	}
	matched := make(map[string]bool, len(membership))
	for _, f := range fs {
		if f.LibraryID != lib.ID {
			continue
		}
		if filesSeen[f.ID] {
			return errors.New("file assigned to multiple catalog items")
		}
		filesSeen[f.ID] = true
		m, ok := membership[f.ID]
		if !ok {
			inv.Unmatched[item.ID] = append(inv.Unmatched[item.ID], f)
			continue
		}
		matched[f.ID] = true
		if f.Path == "" || !filepath.IsAbs(f.Path) || strings.Contains(f.Path, "\x00") {
			inv.Refusals[item.ID] = "nonlocal or hidden media path"
			continue
		}
		m.File = f
		inv.Files = append(inv.Files, m)
	}
	for id := range membership {
		if !matched[id] {
			inv.Refusals[item.ID] = "catalog file missing from selected library admin inventory"
			break
		}
	}
	return nil
}

// Discovered checks a specific filename-derived title and exact native owner.
func (c *Client) Discovered(ctx context.Context, owner, library, title string) (bool, error) {
	d, e := c.Detail(ctx, owner, library)
	if e != nil {
		return false, e
	}
	if d.Themes == nil || d.Themes.OwnerID != owner {
		return false, nil
	}
	count := 0
	for _, s := range d.Themes.Items {
		if s.Title == title {
			count++
		}
	}
	return count == 1, nil
}
