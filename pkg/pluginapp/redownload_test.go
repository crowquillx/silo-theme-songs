package pluginapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crowquillx/silo-theme-songs/pkg/engine"
)

func ptr[T any](v T) *T { return &v }

func themeFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "theme-music"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if audioName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestLoudnessSettings(t *testing.T) {
	for _, tc := range []struct {
		value   Loudness
		target  float64
		profile string
		tag     string
	}{
		{"", -16, "loudness:-16", "-lufs16"},
		{"off", 0, "", ""},
		{"OFF", 0, "", ""},
		{"-23", -23, "loudness:-23", "-lufs23"},
		{"−14", -14, "loudness:-14", "-lufs14"},
	} {
		c := Config{ThemeLoudness: tc.value}
		if c.LoudnessTarget() != tc.target || c.audioProfile() != tc.profile || profileTag(c.audioProfile()) != tc.tag {
			t.Errorf("%q: target %v profile %q tag %q", tc.value, c.LoudnessTarget(), c.audioProfile(), profileTag(c.audioProfile()))
		}
	}
	for _, bad := range []Loudness{"-9", "-31", "-16.5", "loud"} {
		if _, err := bad.target(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	cfg, err := parseSettings(map[string]any{"api_key": "k", "theme_loudness": float64(-18), "redownload_mismatched": true})
	if err != nil || cfg.LoudnessTarget() != -18 || !cfg.redownload() {
		t.Fatalf("numeric setting: %+v %v", cfg, err)
	}
	cfg, err = parseSettings(map[string]any{"api_key": "k", "theme_loudness": "-20"})
	if err != nil || cfg.LoudnessTarget() != -20 || cfg.redownload() {
		t.Fatalf("form setting: %+v %v", cfg, err)
	}
}

// A volume change leaves existing themes alone until redownload is on. Then
// the theme is replaced, and the old file goes once the new one is saved.
func TestRedownloadReplacesThemeWithOtherVolume(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	movie := silo.addMovie("movie:1", "Movie")
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	cfg.ThemeLoudness = "off"
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if r := s.Execute(ctx, "sync", nil); r.Downloaded != 1 {
		t.Fatalf("first run: %+v", r)
	}
	original := themeFiles(t, movie.dir)
	if len(original) != 1 || strings.Contains(original[0], "lufs") {
		t.Fatalf("files %v", original)
	}

	cfg.ThemeLoudness = "-16"
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if r := s.Execute(ctx, "sync", nil); r.Downloaded != 0 || p.calls.Load() != 1 {
		t.Fatalf("replaced without redownload: %+v", r)
	}

	cfg.Redownload = ptr(true)
	cfg.DryRun = ptr(true)
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	r := s.Execute(ctx, "sync", nil)
	if !hasAction(r.Rows, "would_download") || !hasAction(r.Rows, "would_remove") || p.calls.Load() != 1 {
		t.Fatalf("dry run: %+v", r.Rows)
	}

	cfg.DryRun = ptr(false)
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	r = s.Execute(ctx, "sync", nil)
	if r.Downloaded != 1 || !hasAction(r.Rows, "removed") || r.State != "complete" {
		t.Fatalf("replacement: %+v", r)
	}
	files := themeFiles(t, movie.dir)
	if len(files) != 1 || !strings.HasSuffix(files[0], "-lufs16.mp3") {
		t.Fatalf("files %v", files)
	}
	records := s.general.engine.Records()
	if len(records) != 1 || records[0].Audio != "loudness:-16" || records[0].Status != engine.Discovered {
		t.Fatalf("records %+v", records)
	}
	if scans := silo.scanned(); scans[len(scans)-1] != filepath.Join(movie.dir, "theme-music") {
		t.Fatalf("old theme not rescanned: %v", scans)
	}
	if r := s.Execute(ctx, "sync", nil); r.Downloaded != 0 || hasAction(r.Rows, "removed") {
		t.Fatalf("repeat run: %+v", r)
	}
}

func TestRedownloadReplacesChangedSource(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	movie := silo.addMovie("movie:1", "Movie")
	p := &fakeProvider{candidates: []Candidate{{ID: "fixture-1", Title: "Old", Extension: "ogg", Provenance: "fixture"}}}
	s, cfg := newTestServer(t, silo, p, nil)
	p.staged = stagedAudio(t)
	cfg.Redownload = ptr(true)
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if r := s.Execute(ctx, "sync", nil); r.Downloaded != 1 {
		t.Fatalf("first run: %+v", r)
	}
	p.candidates = []Candidate{{ID: "fixture-2", Title: "New", Extension: "ogg", Provenance: "fixture"}}
	r := s.Execute(ctx, "sync", nil)
	if r.Downloaded != 1 || !hasAction(r.Rows, "removed") || hasAction(r.Rows, "selection_changed") {
		t.Fatalf("replacement: %+v", r.Rows)
	}
	records := s.general.engine.Records()
	if files := themeFiles(t, movie.dir); len(records) != 1 || records[0].SourceID != "fixture-2" || len(files) != 1 || !strings.Contains(files[0], "-new-") {
		t.Fatalf("records %+v files %v", records, files)
	}
}

// A replacement that fails to download keeps the old theme playing.
func TestFailedRedownloadKeepsOldTheme(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "movies")
	movie := silo.addMovie("movie:1", "Movie")
	p := &fakeProvider{staged: stagedAudio(t)}
	s, cfg := newTestServer(t, silo, p, nil)
	cfg.ThemeLoudness = "off"
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	s.Execute(ctx, "sync", nil)
	cfg.ThemeLoudness, cfg.Redownload = "-14", ptr(true)
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	p.fetchErr = errors.New("source offline")
	r := s.Execute(ctx, "sync", nil)
	if !hasAction(r.Rows, "download_failed") || hasAction(r.Rows, "removed") {
		t.Fatalf("rows %+v", r.Rows)
	}
	if files := themeFiles(t, movie.dir); len(files) != 1 || strings.Contains(files[0], "lufs") {
		t.Fatalf("files %v", files)
	}
}

// AnimeThemes themes the settings no longer select are removed once the
// themes they do select are saved. A season with no selection keeps its own.
func TestRedownloadRemovesDeselectedAnimeThemes(t *testing.T) {
	ctx := context.Background()
	silo := newFakeSilo(t, "series")
	silo.addSeries("series:1", "Show", "100")
	anime := &fakeProvider{claims: map[string]bool{"series:1": true}, candidates: []Candidate{{ID: "op1", Title: "OP1", Extension: "ogg"}, {ID: "ed1", Title: "ED1", Extension: "ogg"}}}
	s, cfg := newTestServer(t, silo, &fakeProvider{}, anime)
	anime.staged = stagedAudio(t)
	cfg.Redownload = ptr(true)
	if err := s.ConfigureLocal(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if r := s.Execute(ctx, "sync", nil); r.Downloaded != 2 {
		t.Fatalf("first run: %+v", r)
	}
	season := filepath.Join(silo.root, "Show", "Season 01")
	anime.candidates = anime.candidates[:1]
	r := s.Execute(ctx, "sync", nil)
	if r.Downloaded != 0 || !hasAction(r.Rows, "removed") {
		t.Fatalf("rows %+v", r.Rows)
	}
	if files := themeFiles(t, season); len(files) != 1 || !strings.Contains(files[0], "op1") {
		t.Fatalf("files %v", files)
	}
	anime.candidates = []Candidate{}
	if r := s.Execute(ctx, "sync", nil); hasAction(r.Rows, "removed") {
		t.Fatalf("removed with nothing selected: %+v", r.Rows)
	}
	if files := themeFiles(t, season); len(files) != 1 {
		t.Fatalf("files %v", files)
	}
}
