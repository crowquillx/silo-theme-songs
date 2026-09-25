package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestThemerrDBExactPathsAndCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/ThemerrDB/tv_shows/themoviedb/123.json":
			io.WriteString(w, `{"youtube_theme_url":"https://youtu.be/abcdefghijk"}`)
		case "/ThemerrDB/movies/themoviedb/456.json":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	db := &ThemerrDB{BaseURL: server.URL + "/ThemerrDB"}
	for range 2 {
		url, err := db.Lookup(context.Background(), TV, "123")
		if err != nil || url != "https://youtu.be/abcdefghijk" {
			t.Fatalf("TV: %q %v", url, err)
		}
		_, err = db.Lookup(context.Background(), Movie, "456")
		if !IsCode(err, Absent) {
			t.Fatalf("movie absence: %v", err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("cache calls = %d", got)
	}
}

func TestThemerrDBMalformedAndTransientAreNotAbsence(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv_shows/themoviedb/1.json":
			io.WriteString(w, `{"youtube_theme_url":42}`)
		case "/tv_shows/themoviedb/2.json":
			calls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/tv_shows/themoviedb/3.json":
			io.WriteString(w, strings.Repeat("x", 129<<10))
		}
	}))
	defer server.Close()
	db := &ThemerrDB{BaseURL: server.URL}
	for _, tc := range []struct {
		id   string
		want Code
	}{{"1", Malformed}, {"2", Transient}, {"3", Malformed}} {
		_, err := db.Lookup(context.Background(), TV, tc.id)
		if !IsCode(err, tc.want) {
			t.Fatalf("id=%s: %v", tc.id, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("transient retries = %d", calls.Load())
	}
}

func TestCuratedBadURLIsMalformed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"youtube_theme_url":"https://evil.example/a.mp3"}`)
	}))
	defer server.Close()
	r := &Resolver{DB: &ThemerrDB{BaseURL: server.URL}, AllowedDirectHosts: []string{"evil.example"}}
	_, err := r.Resolve(context.Background(), Request{Kind: TV, IDs: IDs{TMDB: "1"}})
	if !IsCode(err, Malformed) {
		t.Fatalf("curated URL: %v", err)
	}
}

func TestResolverSelectionAndURLPolicy(t *testing.T) {
	r := &Resolver{AllowedDirectHosts: []string{"audio.example.org"}}
	for _, tc := range []struct {
		req            Request
		origin, format string
		extract        bool
	}{
		{Request{Kind: TV, OverrideURL: "https://youtu.be/abcdefghijk"}, "override", "mp3", true},
		{Request{Kind: Movie, IDs: IDs{TMDB: "77"}, Template: "https://audio.example.org/movie/{tmdbId}.ogg"}, "template", "ogg", false},
		{Request{Kind: TV, IDs: IDs{TVDB: "88"}, Template: "https://audio.example.org/tv/{tvdbId}.flac"}, "template", "flac", false},
	} {
		s, err := r.Resolve(context.Background(), tc.req)
		if err != nil || s.Origin != tc.origin || s.Format != tc.format || s.Extract != tc.extract {
			t.Fatalf("source=%+v err=%v", s, err)
		}
	}
	for _, tc := range []struct {
		req  Request
		want Code
	}{
		{Request{Kind: TV, IDs: IDs{TVDB: "88"}}, MissingID},
		{Request{Kind: TV, OverrideURL: "http://youtu.be/abcdefghijk"}, UnsafeURL},
		{Request{Kind: TV, OverrideURL: "https://youtube.com/watch?v=abcdefghijk&list=evil"}, UnsafeURL},
		{Request{Kind: TV, OverrideURL: "https://audio.example.org@127.0.0.1/a.mp3"}, UnsafeURL},
		{Request{Kind: TV, OverrideURL: "https://audio.example.org.evil.test/a.mp3"}, UnsafeURL},
		{Request{Kind: TV, IDs: IDs{TMDB: "1"}, Template: "https://{tmdbId}.example.org/a.mp3"}, UnsafeURL},
		{Request{Kind: TV, Template: "https://audio.example.org/a/{unknown}.mp3"}, Malformed},
	} {
		_, err := r.Resolve(context.Background(), tc.req)
		if !IsCode(err, tc.want) {
			t.Fatalf("request=%+v err=%v", tc.req, err)
		}
	}
}

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func tools(t *testing.T, ytBody, probeBody string) ToolPaths {
	t.Helper()
	dir := t.TempDir()
	yt := script(t, dir, "yt-dlp", "if [ \"$1\" = '--version' ]; then echo 2026.09.01; exit 0; fi\n"+ytBody)
	ffmpeg := script(t, dir, "ffmpeg", "echo 'ffmpeg version 7.1'\n")
	probe := script(t, dir, "ffprobe", "if [ \"$1\" = '-version' ]; then echo 'ffprobe version 7.1'; exit 0; fi\n"+probeBody)
	return ToolPaths{YTDLP: yt, FFmpeg: ffmpeg, FFprobe: probe, JSRuntime: "node:" + script(t, dir, "node", "echo v22.0.0")}
}

func TestExtractionArgumentsStagingAndAudioProbe(t *testing.T) {
	stage := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	ytBody := "printf '%s\\n' \"$@\" > '" + argsFile + "'\nprintf audio > theme.mp3"
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: tools(t, ytBody, "echo '{\"format\":{\"format_name\":\"mp3\"},\"streams\":[{\"codec_type\":\"audio\",\"codec_name\":\"mp3\",\"channels\":2}]}'"), StageParent: stage}
	s, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Origin: "themerrdb", Format: "mp3", Extract: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Size != 5 || filepath.Dir(s.Path) == stage {
		t.Fatalf("stage=%+v", s)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--ignore-config", "--no-playlist", "--no-plugin-dirs", "--max-filesize", "--use-extractors", "--audio-format", "192K"} {
		if !strings.Contains(string(args), flag) {
			t.Fatalf("missing %s: %s", flag, args)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage survived close: %v", err)
	}
}

func TestExtractionRejectsVideoAndCleansStage(t *testing.T) {
	stage := t.TempDir()
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: tools(t, "printf audio > theme.mp3", "echo '{\"format\":{\"format_name\":\"mp3\"},\"streams\":[{\"codec_type\":\"audio\",\"codec_name\":\"mp3\",\"channels\":2},{\"codec_type\":\"video\"}]}'"), StageParent: stage}
	_, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
	if !IsCode(err, InvalidAudio) {
		t.Fatalf("error=%v", err)
	}
	entries, _ := os.ReadDir(stage)
	if len(entries) != 0 {
		t.Fatalf("left staging: %v", entries)
	}
}

func TestProbeRejectsAudioContainerMismatchAndCleansStage(t *testing.T) {
	stage := t.TempDir()
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: tools(t, "printf audio > theme.mp3", "echo '{\"format\":{\"format_name\":\"ogg\"},\"streams\":[{\"codec_type\":\"audio\",\"codec_name\":\"mp3\",\"channels\":2}]}'"), StageParent: stage}
	_, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
	if !IsCode(err, InvalidAudio) {
		t.Fatalf("mismatched container accepted: %v", err)
	}
	entries, err := os.ReadDir(stage)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging left after rejection: %v %v", entries, err)
	}
}

func TestExtractionResolvesFFmpegOnPath(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	toolPaths := tools(t, "printf '%s\\n' \"$@\" > '"+argsFile+"'\nprintf audio > theme.mp3", "echo '{\"format\":{\"format_name\":\"mp3\"},\"streams\":[{\"codec_type\":\"audio\",\"codec_name\":\"mp3\",\"channels\":2}]}'")
	t.Setenv("PATH", filepath.Dir(toolPaths.FFmpeg)+":/usr/bin:/bin")
	toolPaths.FFmpeg = "ffmpeg"
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: toolPaths, StageParent: t.TempDir()}
	staged, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), filepath.Join(filepath.Dir(toolPaths.YTDLP), "ffmpeg")) {
		t.Fatalf("ffmpeg path was not resolved: %s", args)
	}
}

func TestExtractionBoundsAndUnavailableOutput(t *testing.T) {
	for _, tc := range []struct {
		body string
		max  int64
		code Code
	}{
		{"yes x | head -c 9000; printf audio > theme.mp3", 100, LimitExceeded},
		{"dd if=/dev/zero of=theme.mp3 bs=1024 count=2 2>/dev/null", 8, LimitExceeded},
		{"exit 0", 100, UnavailableMedia},
	} {
		stage := t.TempDir()
		d := &Downloader{youtube: newYouTubeLimiter(0), Tools: tools(t, tc.body, "echo '{\"format\":{\"format_name\":\"mp3\"},\"streams\":[{\"codec_type\":\"audio\",\"codec_name\":\"mp3\",\"channels\":2}]}'"), StageParent: stage, MaxBytes: tc.max}
		_, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
		if !IsCode(err, tc.code) {
			t.Fatalf("body=%q err=%v", tc.body, err)
		}
		entries, _ := os.ReadDir(stage)
		if len(entries) != 0 {
			t.Fatalf("staging survived: %v", entries)
		}
	}
}

func TestMissingToolsAndCircuitBreaker(t *testing.T) {
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: ToolPaths{}, StageParent: t.TempDir()}
	_, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
	if !IsCode(err, MissingTool) {
		t.Fatalf("missing tools: %v", err)
	}
	d.Tools = tools(t, "exit 1", "echo '{\"format\":{\"format_name\":\"mp3\"},\"streams\":[{\"codec_type\":\"audio\",\"codec_name\":\"mp3\",\"channels\":2}]}'")
	for _, url := range []string{"https://youtu.be/abcdefghijk", "https://youtu.be/bcdefghijkl", "https://youtu.be/cdefghijklm"} {
		_, err = d.Fetch(context.Background(), Source{URL: url, Format: "mp3", Extract: true})
		if !IsCode(err, UnavailableMedia) {
			t.Fatalf("failure=%v", err)
		}
	}
	_, err = d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
	if !IsCode(err, ExtractorBroken) {
		t.Fatalf("circuit=%v", err)
	}
}

func TestProcessGroupCancellation(t *testing.T) {
	stage := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child-finished")
	ytBody := "(sleep 0.5; touch '" + marker + "') &\nsleep 5"
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: tools(t, ytBody, "echo '{\"format\":{\"format_name\":\"mp3\"},\"streams\":[{\"codec_type\":\"audio\",\"codec_name\":\"mp3\",\"channels\":2}]}'"), StageParent: stage}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := d.Fetch(ctx, Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation=%v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subprocess survived: %v", err)
	}
	entries, _ := os.ReadDir(stage)
	if len(entries) != 0 {
		t.Fatalf("left staging: %v", entries)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDirectDownloadBoundsAndRedirect(t *testing.T) {
	status := http.StatusOK
	body := "audio"
	contentType := "audio/mpeg"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}, "Location": []string{"https://evil.example/out.mp3"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
	})}
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: tools(t, "exit 1", "echo '{\"format\":{\"format_name\":\"mp3\"},\"streams\":[{\"codec_type\":\"audio\",\"codec_name\":\"mp3\",\"channels\":2}]}'"), StageParent: t.TempDir(), Client: client, AllowedDirectHosts: []string{"audio.example.org"}, MaxBytes: 8}
	s := Source{URL: "https://audio.example.org/theme.mp3", Format: "mp3"}
	staged, err := d.Fetch(context.Background(), s)
	if err != nil || staged.Size != 5 {
		t.Fatalf("direct stage=%+v err=%v", staged, err)
	}
	staged.Close()
	body = "<html>bad</html>"
	contentType = "text/html"
	_, err = d.Fetch(context.Background(), s)
	if !IsCode(err, LimitExceeded) {
		t.Fatalf("oversize=%v", err)
	}
	body = "error"
	_, err = d.Fetch(context.Background(), s)
	if !IsCode(err, InvalidAudio) {
		t.Fatalf("HTML=%v", err)
	}
	status = http.StatusFound
	_, err = d.Fetch(context.Background(), s)
	if !IsCode(err, UnsafeURL) {
		t.Fatalf("redirect=%v", err)
	}
	_, err = d.Fetch(context.Background(), Source{URL: "https://evil.example/a.mp3", Format: "mp3"})
	if !IsCode(err, UnsafeURL) {
		t.Fatalf("host bypass=%v", err)
	}
}
