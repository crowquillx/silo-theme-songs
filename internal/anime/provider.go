// Package anime adapts anime mapping and AnimeThemes metadata to pluginapp.Provider.
package anime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-theme-songs/internal/animethemes"
	"github.com/crowquillx/silo-theme-songs/internal/mapping"
	"github.com/crowquillx/silo-theme-songs/pkg/pluginapp"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
)

// ProviderConfig is the JSON value in pluginapp.Config.AnimeProvider. ManualMappings
// keys are "tvdb_show:<id>:s<season>" or "tmdb_show:<id>:s<season>".
// SeriesFallback keys are Silo series IDs and apply only to series targets.
type ProviderConfig struct {
	AniBridgeURL       string              `json:"anibridge_url"`
	AnimeListsURL      string              `json:"anime_lists_url"`
	MappingUpdateHours int                 `json:"mapping_update_hours"`
	OP                 *bool               `json:"op"`
	ED                 *bool               `json:"ed"`
	AllDistinct        bool                `json:"all_distinct"`
	AllowSpoiler       bool                `json:"allow_spoiler"`
	AllowNSFW          bool                `json:"allow_nsfw"`
	AllowOverlap       bool                `json:"allow_overlap"`
	ManualMappings     map[string][]string `json:"manual_mappings"`
	SeriesFallback     map[string][]string `json:"series_fallback"`
	SelectionOverrides map[string][]string `json:"selection_overrides"`
	AudioHosts         []string            `json:"audio_hosts"`
	FFmpeg             string              `json:"ffmpeg"`
}
type Adapter struct {
	config      ProviderConfig
	stateDir    string
	mapping     *mapping.Store
	themes      *animethemes.Client
	downloader  *provider.Downloader
	mu          sync.Mutex
	lastRefresh time.Time
}

// New is the pluginapp.Factory for this binary. The parent supplies persistent
// stateDir and validates destination ownership before publishing staged audio.
func New(c pluginapp.Config) (pluginapp.Provider, error) {
	cfg := ProviderConfig{FFmpeg: provider.BundledTool("ffmpeg")}
	if len(c.AnimeProvider) > 0 {
		if e := json.Unmarshal(c.AnimeProvider, &cfg); e != nil {
			return nil, fmt.Errorf("anime provider config: %w", e)
		}
	}
	// The settings form's switches override the provider JSON.
	if c.AnimeOpenings != nil {
		cfg.OP = c.AnimeOpenings
	}
	if c.AnimeEndings != nil {
		cfg.ED = c.AnimeEndings
	}
	if c.AnimeSpoilers != nil {
		cfg.AllowSpoiler = *c.AnimeSpoilers
	}
	if c.AnimeNSFW != nil {
		cfg.AllowNSFW = *c.AnimeNSFW
	}
	if !filepath.IsAbs(c.StateDir) || c.StateDir == "/" {
		return nil, errors.New("anime provider requires absolute state_dir")
	}
	if cfg.MappingUpdateHours < 0 || cfg.MappingUpdateHours > 24*30 {
		return nil, errors.New("mapping_update_hours out of range")
	}
	if len(cfg.AudioHosts) == 0 {
		cfg.AudioHosts = []string{"a.animethemes.moe"}
	}
	for _, h := range cfg.AudioHosts {
		if h == "" || strings.ContainsAny(h, "/:@ ") {
			return nil, errors.New("invalid audio host")
		}
	}
	if e := os.MkdirAll(c.StateDir, 0700); e != nil {
		return nil, e
	}
	m := mapping.NewStore(nil)
	for _, v := range []struct {
		name string
		load func([]byte) (mapping.Source, error)
	}{{"anibridge-v3.json", m.LoadAniBridge}, {"anime-lists.xml", m.LoadAnimeLists}} {
		data, e := readCached(filepath.Join(c.StateDir, v.name))
		if e == nil {
			if _, e = v.load(data); e != nil {
				return nil, fmt.Errorf("cached %s: %w", v.name, e)
			}
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	return &Adapter{config: cfg, stateDir: c.StateDir, mapping: m, themes: animethemes.NewClient(nil), downloader: &provider.Downloader{Tools: provider.ToolPaths{FFprobe: provider.DefaultTool(c.FFprobe, "ffprobe"), FFmpeg: provider.DefaultTool(cfg.FFmpeg, "ffmpeg")}, StageParent: c.StateDir, AllowedDirectHosts: cfg.AudioHosts}}, nil
}
func readCached(path string) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, mapping.MaxSnapshotBytes+1))
	if e != nil {
		return nil, e
	}
	if len(data) > mapping.MaxSnapshotBytes {
		return nil, errors.New("cached mapping exceeds size limit")
	}
	return data, nil
}
func enabled(v *bool) bool { return v == nil || *v }
func (a *Adapter) refresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	interval := time.Duration(a.config.MappingUpdateHours) * time.Hour
	if interval == 0 {
		interval = 24 * time.Hour
	}
	if !a.lastRefresh.IsZero() && time.Since(a.lastRefresh) < interval {
		return nil
	}
	// Each source updates separately. A failed fetch never replaces its last
	// valid in-memory snapshot or its cached source bytes.
	var failures, cacheFailures []error
	for _, v := range []struct {
		name, url string
		update    func(context.Context, string) (mapping.Source, []byte, error)
	}{{"anibridge-v3.json", a.config.AniBridgeURL, a.mapping.UpdateAniBridge}, {"anime-lists.xml", a.config.AnimeListsURL, a.mapping.UpdateAnimeLists}} {
		_, data, e := v.update(ctx, v.url)
		if e != nil {
			failures = append(failures, fmt.Errorf("%s: %w", v.name, e))
			continue
		}
		if e = saveSnapshot(a.stateDir, v.name, data); e != nil {
			cacheFailures = append(cacheFailures, fmt.Errorf("%s cache: %w", v.name, e))
		}
	}
	if len(cacheFailures) > 0 {
		return errors.Join(cacheFailures...)
	}
	if !a.mapping.HasSnapshots() {
		return errors.Join(failures...)
	}
	a.lastRefresh = time.Now()
	return nil
}
func saveSnapshot(dir, name string, data []byte) error {
	f, e := os.CreateTemp(dir, ".mapping-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(dir, name)); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func targetKey(t pluginapp.Target) string {
	if t.Season == nil {
		return "series:" + t.Item.ID
	}
	return fmt.Sprintf("%s:s%d", t.Item.ID, t.Season.Number)
}
func mapKey(provider, id string, season int) string {
	return fmt.Sprintf("%s:%s:s%d", provider, id, season)
}
func (a *Adapter) resolve(t pluginapp.Target) (mapping.Result, error) {
	if t.Season == nil {
		ids := a.config.SeriesFallback[t.Item.ID]
		if len(ids) == 0 {
			return mapping.Result{Status: mapping.Missing}, nil
		}
		matches := make([]mapping.Match, 0, len(ids))
		for _, id := range ids {
			if n, e := strconv.Atoi(id); e != nil || n <= 0 {
				return mapping.Result{}, errors.New("invalid series fallback AniDB ID")
			}
			matches = append(matches, mapping.Match{AniDBID: id, Source: "manual", CoverageUnknown: true})
		}
		return mapping.Result{Status: mapping.Resolved, Matches: matches, Provenance: mapping.Source{Version: "manual"}}, nil
	}
	r := a.mapping.Resolver()
	n := t.Season.Number
	// Explicit mappings take precedence regardless of the configured provider.
	for _, p := range []struct{ kind, id string }{{"tvdb_show", t.Item.TVDB}, {"tmdb_show", t.Item.TMDB}} {
		if p.id == "" {
			continue
		}
		if ids, ok := a.config.ManualMappings[mapKey(p.kind, p.id, n)]; ok {
			if len(ids) == 0 {
				return mapping.Result{Status: mapping.Missing, Reason: "manual mapping disabled"}, nil
			}
			return r.Resolve(mapping.Query{Provider: p.kind, ID: p.id, Season: n, ManualAniDBIDs: ids}), nil
		}
	}
	results := []mapping.Result{}
	for _, p := range []struct{ kind, id string }{{"tvdb_show", t.Item.TVDB}, {"tmdb_show", t.Item.TMDB}} {
		if p.id == "" {
			continue
		}
		// Season themes include every mapped cour. Target.Episodes can describe
		// only the media currently present and must not narrow the season set.
		got := r.Resolve(mapping.Query{Provider: p.kind, ID: p.id, Season: n})
		if got.Status != mapping.Missing {
			results = append(results, got)
		}
	}
	if len(results) == 0 {
		return mapping.Result{Status: mapping.Missing}, nil
	}
	slices.SortStableFunc(results, func(x, y mapping.Result) int {
		if x.Provenance.Version == "anime-lists-xml-v1" && y.Provenance.Version != "anime-lists-xml-v1" {
			return 1
		}
		if y.Provenance.Version == "anime-lists-xml-v1" && x.Provenance.Version != "anime-lists-xml-v1" {
			return -1
		}
		return 0
	})
	best := results[0]
	if best.Status == mapping.Ambiguous {
		return best, nil
	}
	for _, other := range results[1:] {
		if other.Provenance.Version != best.Provenance.Version {
			continue
		}
		if other.Status == mapping.Ambiguous {
			return other, nil
		}
		if !sameIDs(best.Matches, other.Matches) {
			return mapping.Result{Status: mapping.Ambiguous, Reason: "TVDB and TMDB mappings disagree"}, nil
		}
	}
	return best, nil
}
func sameIDs(a, b []mapping.Match) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].AniDBID != b[i].AniDBID || a[i].Scope != b[i].Scope {
			return false
		}
	}
	return true
}

// Claims reports whether AnimeThemes handles a series: whether it has a
// manual series fallback or maps any of its seasons, even ambiguously.
func (a *Adapter) Claims(ctx context.Context, targets []pluginapp.Target) (bool, error) {
	for _, t := range targets {
		if t.Season == nil && len(a.config.SeriesFallback[t.Item.ID]) > 0 {
			return true, nil
		}
	}
	if e := a.refresh(ctx); e != nil {
		return false, e
	}
	for _, t := range targets {
		if t.Season == nil {
			continue
		}
		mapped, e := a.resolve(t)
		if e != nil {
			return false, e
		}
		if mapped.Status != mapping.Missing {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) Select(ctx context.Context, t pluginapp.Target) ([]pluginapp.Candidate, error) {
	if t.Season == nil && len(a.config.SeriesFallback[t.Item.ID]) == 0 {
		return nil, nil
	}
	if e := a.refresh(ctx); e != nil {
		return nil, e
	}
	mapped, e := a.resolve(t)
	if e != nil {
		return nil, e
	}
	if mapped.Status == mapping.Ambiguous {
		return nil, fmt.Errorf("anime mapping ambiguous: %s", mapped.Reason)
	}
	if mapped.Status != mapping.Resolved {
		return nil, nil
	}
	ids := make([]int, 0, len(mapped.Matches))
	for _, m := range mapped.Matches {
		id, e := strconv.Atoi(m.AniDBID)
		if e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	found, e := a.themes.LookupAniDB(ctx, ids)
	if e != nil {
		return nil, e
	}
	all := []animethemes.Anime{}
	for _, id := range ids {
		all = append(all, found[id]...)
	}
	override := a.config.SelectionOverrides[targetKey(t)]
	opts := animethemes.Options{OP: enabled(a.config.OP), ED: enabled(a.config.ED), AllDistinct: a.config.AllDistinct || len(override) > 0, AllowSpoiler: a.config.AllowSpoiler, AllowNSFW: a.config.AllowNSFW, AllowOverlap: a.config.AllowOverlap}
	selected := animethemes.Select(all, opts)
	allowed := map[string]bool{}
	for _, id := range override {
		allowed[id] = true
	}
	out := make([]pluginapp.Candidate, 0, len(selected.Candidates))
	provenanceByID := map[int]string{}
	for _, m := range mapped.Matches {
		n, _ := strconv.Atoi(m.AniDBID)
		provenanceByID[n] = m.Source
	}
	for _, c := range selected.Candidates {
		id := fmt.Sprintf("animethemes-%d-%d", c.ThemeID, c.AudioID)
		if len(override) > 0 && !allowed[id] {
			continue
		}
		title := c.Title
		if title == "" {
			title = fmt.Sprintf("%s%d", c.Type, c.Sequence)
		}
		out = append(out, pluginapp.Candidate{ID: id, Title: title, URL: c.URL, Extension: c.Extension, Provenance: fmt.Sprintf("%s:%s;anidb:%d;anime:%d", provenanceByID[c.AniDBID], mapped.Provenance.SHA256, c.AniDBID, c.AnimeID)})
	}
	if len(override) > 0 {
		found := map[string]bool{}
		for _, c := range out {
			found[c.ID] = true
		}
		for _, id := range override {
			if !found[id] {
				return nil, fmt.Errorf("selection override %q unavailable or blocked by safety filters", id)
			}
		}
	}
	return out, nil
}
func (a *Adapter) Fetch(ctx context.Context, c pluginapp.Candidate) (*provider.StagedAudio, error) {
	if c.Extract {
		return nil, errors.New("AnimeThemes audio cannot require extraction")
	}
	return a.downloader.Fetch(ctx, provider.Source{URL: c.URL, Origin: "override", Format: c.Extension})
}

// Preflight lets the runtime refuse download readiness before Fetch when the
// configured probing or conversion tools are missing or unsupported.

func (a *Adapter) Preflight(ctx context.Context, c pluginapp.Candidate) error {
	source, err := (&provider.Resolver{AllowedDirectHosts: a.downloader.AllowedDirectHosts}).Resolve(ctx, provider.Request{Kind: provider.TV, OverrideURL: c.URL})
	if err != nil {
		return err
	}
	if source.Extract || source.Format != c.Extension || c.Extract {
		return &provider.Error{Code: provider.UnsafeURL, Op: "anime audio"}
	}
	_, err = a.downloader.Preflight(ctx, source)
	return err
}

var _ pluginapp.Factory = New
var _ pluginapp.Provider = (*Adapter)(nil)
var _ pluginapp.Claimer = (*Adapter)(nil)
