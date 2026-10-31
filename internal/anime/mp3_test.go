package anime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/pluginapp"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
)

type audioTransport func(*http.Request) (*http.Response, error)

func (f audioTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAdapterConvertsOggToMP3(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	ffprobe, probeErr := exec.LookPath("ffprobe")
	if err != nil || probeErr != nil {
		if os.Getenv("SILO_REQUIRE_FFMPEG") == "1" {
			t.Fatal("FFmpeg and ffprobe required")
		}
		t.Skip("install FFmpeg and ffprobe for real adapter conversion")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := filepath.Join(t.TempDir(), "source.ogg")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=duration=0.3", "-c:a", "libvorbis", input).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	body, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(ProviderConfig{FFmpeg: ffmpeg})
	p, err := New(pluginapp.Config{StateDir: t.TempDir(), FFprobe: ffprobe, AnimeProvider: settings})
	if err != nil {
		t.Fatal(err)
	}
	a := p.(*Adapter)
	a.downloader.Client = &http.Client{Transport: audioTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
	})}
	candidate := pluginapp.Candidate{ID: "animethemes-1-2", URL: "https://a.animethemes.moe/fixture.ogg", Extension: "ogg"}
	if err := a.Preflight(ctx, candidate); err != nil {
		t.Fatal(err)
	}
	staged, err := a.Fetch(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	if staged.Format != "mp3" || filepath.Ext(staged.Path) != ".mp3" {
		t.Fatalf("wrong output: %+v", staged)
	}
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_name:format=format_name", "-of", "json", staged.Path).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte(`"codec_name": "mp3"`)) || !bytes.Contains(out, []byte(`"format_name": "mp3"`)) {
		t.Fatalf("output is not MP3: %v %s", err, out)
	}
	a.downloader.Tools.FFmpeg = "/nonexistent/ffmpeg"
	if err := a.Preflight(ctx, candidate); !provider.IsCode(err, provider.MissingTool) {
		t.Fatalf("missing conversion dependency not reported: %v", err)
	}
}
