package provider

import (
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
