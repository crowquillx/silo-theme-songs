package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultToolPrefersSiloBundle(t *testing.T) {
	old := bundledFFmpegDir
	t.Cleanup(func() { bundledFFmpegDir = old })
	bundledFFmpegDir = t.TempDir()
	t.Setenv("PATH", t.TempDir())
	bundled := filepath.Join(bundledFFmpegDir, "ffmpeg")
	if got := DefaultTool("", "ffmpeg"); got != "ffmpeg" {
		t.Fatalf("missing bundle resolved to %q", got)
	}
	if e := os.WriteFile(bundled, []byte("#!/bin/sh\n"), 0755); e != nil {
		t.Fatal(e)
	}
	for _, setting := range []string{"", "ffmpeg", bundled} {
		if got := DefaultTool(setting, "ffmpeg"); got != bundled {
			t.Fatalf("%q resolved to %q, want the Silo bundle", setting, got)
		}
	}
	if got := DefaultTool("/opt/ffmpeg/bin/ffmpeg", "ffmpeg"); got != "/opt/ffmpeg/bin/ffmpeg" {
		t.Fatalf("explicit path replaced with %q", got)
	}
	onPath := t.TempDir()
	if e := os.WriteFile(filepath.Join(onPath, "ffmpeg"), []byte("#!/bin/sh\n"), 0755); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", onPath)
	if got := DefaultTool("ffmpeg", "ffmpeg"); got != "ffmpeg" {
		t.Fatalf("PATH copy replaced with %q", got)
	}
}

type fakeInstaller struct {
	ytdlp, deno string
	calls       int
	stale       bool
}

func (f *fakeInstaller) YTDLP(context.Context) (string, error) { f.calls++; return f.ytdlp, nil }
func (f *fakeInstaller) Deno(context.Context) (string, error)  { f.calls++; return f.deno, nil }
func (f *fakeInstaller) MarkStale()                            { f.stale = true }

func TestInstallerFillsMissingYouTubeTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	paths := tools(t, "", "")
	managed := &fakeInstaller{
		ytdlp: script(t, t.TempDir(), "yt-dlp_linux", "echo 2026.10.21"),
		deno:  script(t, t.TempDir(), "deno", "echo 'deno 2.9.8 (stable, release, x86_64-unknown-linux-gnu)'"),
	}
	d := &Downloader{Tools: ToolPaths{YTDLP: "yt-dlp", FFmpeg: paths.FFmpeg, FFprobe: paths.FFprobe}, Installer: managed}
	v, err := d.Preflight(context.Background(), Source{Format: "mp3", Extract: true})
	if err != nil || v.ytdlpPath != managed.ytdlp || v.YTDLP != "2026.10.21" || v.jsRuntimeArg != "deno:"+managed.deno {
		t.Fatalf("managed tools unused: %+v %v", v, err)
	}
	// Explicit settings are never replaced.
	managed.calls = 0
	d.Tools.YTDLP, d.Tools.JSRuntime = paths.YTDLP, paths.JSRuntime
	if v, err := d.Preflight(context.Background(), Source{Format: "mp3", Extract: true}); err != nil || v.ytdlpPath != paths.YTDLP || managed.calls != 0 {
		t.Fatalf("explicit tools replaced: %+v %v", v, err)
	}
	d.Tools.YTDLP = "/nonexistent/yt-dlp"
	if _, err := d.Preflight(context.Background(), Source{Format: "mp3", Extract: true}); !IsCode(err, MissingTool) || managed.calls != 0 {
		t.Fatalf("missing explicit yt-dlp fell back to managed copy: %v", err)
	}
}
