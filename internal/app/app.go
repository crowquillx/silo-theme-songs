package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/pluginapp"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
)

type Options struct {
	URLs              map[string]string `json:"url_overrides"`
	Template          string            `json:"direct_template"`
	Hosts             []string          `json:"allowed_audio_hosts"`
	YTDLP             string            `json:"yt_dlp"`
	FFmpeg            string            `json:"ffmpeg"`
	JSRuntime         string            `json:"js_runtime"`
	MaxBytes          int64             `json:"max_bytes"`
	TimeoutSeconds    int               `json:"timeout_seconds"`
	Types             []string          `json:"types"`
	AssistedSearch    bool              `json:"assisted_search"`
	SearchSoundtrack  bool              `json:"search_soundtrack"`
	AllowCovers       bool              `json:"allow_covers"`
	AllowInstrumental bool              `json:"allow_instrumental"`
}
type App struct {
	opts       Options
	resolver   *provider.Resolver
	downloader *provider.Downloader
}

func New(c pluginapp.Config) (pluginapp.Provider, error) {
	o := Options{YTDLP: "yt-dlp", FFmpeg: "ffmpeg", MaxBytes: 80 << 20, TimeoutSeconds: 300, Types: []string{"series", "movie"}}
	if len(c.Provider) > 0 {
		d := json.NewDecoder(bytes.NewReader(c.Provider))
		d.DisallowUnknownFields()
		if e := d.Decode(&o); e != nil {
			return nil, errors.New("invalid Theme Songs provider settings")
		}
	}
	if o.MaxBytes < 1 || o.MaxBytes > 512<<20 || o.TimeoutSeconds < 1 || o.TimeoutSeconds > 1200 {
		return nil, errors.New("invalid download size or time limit")
	}
	a := &App{opts: o, resolver: &provider.Resolver{DB: &provider.ThemerrDB{}, AllowedDirectHosts: o.Hosts}, downloader: &provider.Downloader{Tools: provider.ToolPaths{YTDLP: o.YTDLP, FFmpeg: o.FFmpeg, FFprobe: c.FFprobe, JSRuntime: o.JSRuntime}, StageParent: c.StateDir, AllowedDirectHosts: o.Hosts, MaxBytes: o.MaxBytes, MaxDuration: time.Duration(o.TimeoutSeconds) * time.Second}}
	return a, nil
}
func (a *App) Select(ctx context.Context, t pluginapp.Target) ([]pluginapp.Candidate, error) {
	include := false
	for _, kind := range a.opts.Types {
		if kind == t.Item.Type {
			include = true
		}
	}
	if !include {
		return nil, nil
	}
	kind := provider.TV
	if t.Item.Type == "movie" {
		kind = provider.Movie
	}
	s, e := a.resolver.Resolve(ctx, provider.Request{Kind: kind, IDs: provider.IDs{TMDB: t.Item.TMDB, TVDB: t.Item.TVDB, IMDB: t.Item.IMDB}, OverrideURL: a.opts.URLs[t.Item.ID], Template: a.opts.Template})
	if e != nil {
		if a.opts.AssistedSearch && (provider.IsCode(e, provider.Absent) || provider.IsCode(e, provider.MissingID)) && a.opts.URLs[t.Item.ID] == "" && a.opts.Template == "" {
			candidates, searchErr := a.downloader.Search(ctx, provider.SearchOptions{Title: t.Item.Title, Year: t.Item.Year, Soundtrack: a.opts.SearchSoundtrack, AllowCovers: a.opts.AllowCovers, AllowInstrumental: a.opts.AllowInstrumental})
			if searchErr != nil {
				return nil, searchErr
			}
			return nil, &provider.ReviewRequired{Candidates: candidates, Reason: "select a candidate and save its URL as an item override before downloading"}
		}
		return nil, e
	}
	h := sha256.Sum256([]byte(s.URL))
	id := s.Origin + "-" + s.ProviderID + "-" + hex.EncodeToString(h[:10])
	return []pluginapp.Candidate{{ID: id, Title: t.Item.Title, URL: s.URL, Extension: s.Format, Provenance: s.Origin, Extract: s.Extract}}, nil
}
func (a *App) Fetch(ctx context.Context, c pluginapp.Candidate) (*provider.StagedAudio, error) {
	return a.downloader.Fetch(ctx, provider.Source{URL: c.URL, Format: c.Extension, Origin: c.Provenance, Extract: c.Extract})
}
func (a *App) Preflight(ctx context.Context, c pluginapp.Candidate) error {
	_, e := a.downloader.Preflight(ctx, provider.Source{Format: c.Extension, Extract: c.Extract})
	return e
}
