package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixtureDownload(t *testing.T, paths ToolPaths, format string, body []byte) (*Downloader, Source) {
	t.Helper()
	host := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + ".example.org"
	d := &Downloader{Tools: paths, StageParent: t.TempDir(), AllowedDirectHosts: []string{host}, Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
	})}}
	return d, Source{URL: "https://" + host + "/theme." + format, Format: format}
}

func requireFFmpeg(t *testing.T) ToolPaths {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	probe, probeErr := exec.LookPath("ffprobe")
	if err != nil || probeErr != nil {
		if os.Getenv("SILO_REQUIRE_FFMPEG") == "1" {
			t.Fatal("FFmpeg and ffprobe are required")
		}
		t.Skip("install FFmpeg and ffprobe to run real conversion tests")
	}
	return ToolPaths{FFmpeg: ffmpeg, FFprobe: probe}
}

func ffmpegCommand(t *testing.T, binary string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, append([]string{"-nostdin", "-v", "error"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("FFmpeg fixture failed: %v %s", err, out)
	}
	return out
}

func TestRealMP3Downloads(t *testing.T) {
	paths := requireFFmpeg(t)
	for _, tc := range []struct {
		name, ext, codec string
		channels         int
	}{
		{"mp3-pass-through", "mp3", "libmp3lame", 2},
		{"ogg-vorbis", "ogg", "libvorbis", 2},
		{"ogg-opus", "opus", "libopus", 2},
		{"flac", "flac", "flac", 2},
		{"wave", "wav", "pcm_s16le", 2},
		{"aac", "aac", "aac", 2},
		{"m4a", "m4a", "aac", 2},
		{"m4b", "m4b", "aac", 2},
		{"mono", "ogg", "libvorbis", 1},
		{"surround", "flac", "flac", 6},
		{"mp3-in-wave", "wav", "libmp3lame", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "source."+tc.ext)
			args := []string{"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=0.4", "-ac", strconv.Itoa(tc.channels), "-c:a", tc.codec}
			if tc.ext == "m4b" {
				args = append(args, "-f", "mp4")
			}
			ffmpegCommand(t, paths.FFmpeg, append(args, input)...)
			body, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			tools := paths
			if tc.ext == "mp3" {
				tools.FFmpeg = "/nonexistent/ffmpeg" // No encoder dependency or quality loss for MP3.
			}
			d, source := fixtureDownload(t, tools, tc.ext, body)
			staged, err := d.Fetch(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			defer staged.Close()
			if staged.Format != "mp3" || filepath.Ext(staged.Path) != ".mp3" {
				t.Fatalf("wrong output: %+v", staged)
			}
			audio, err := d.probe(context.Background(), staged.Path, "mp3")
			if err != nil || audio.Channels != min(tc.channels, 2) {
				t.Fatalf("invalid output: %+v %v", audio, err)
			}
			out, err := os.ReadFile(staged.Path)
			if err != nil || int64(len(out)) != staged.Size {
				t.Fatalf("incorrect reported size: %v", err)
			}
			if tc.ext == "mp3" && !bytes.Equal(out, body) {
				t.Fatal("valid MP3 was modified")
			}
			if tc.name == "mp3-in-wave" {
				// Hash encoded audio packets, excluding the container and tags.
				hash := func(p string) []byte {
					return ffmpegCommand(t, paths.FFmpeg, "-i", p, "-map", "0:a:0", "-c:a", "copy", "-f", "hash", "-hash", "sha256", "-")
				}
				if !bytes.Equal(hash(input), hash(staged.Path)) {
					t.Fatal("MP3 in another container was re-encoded")
				}
			}
			ffmpegCommand(t, paths.FFmpeg, "-xerror", "-i", staged.Path, "-f", "null", "-")
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
			assertNoStaging(t, d.StageParent)
		})
	}
}

func assertNoStaging(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging not cleaned: %v %v", entries, err)
	}
}

func TestMP3ConversionFailuresCleanStaging(t *testing.T) {
	const valid = `{"format":{"format_name":"mp3"},"streams":[{"codec_type":"audio","codec_name":"mp3","channels":2}]}`
	const ogg = `{"format":{"format_name":"ogg"},"streams":[{"codec_type":"audio","codec_name":"vorbis","channels":2}]}`
	for _, tc := range []struct {
		name, convert, sourceProbe, outputProbe string
		want                                    Code
	}{
		{"failed encoder", "echo 'secret-input-path' >&2; exit 1", ogg, valid, InvalidAudio},
		{"no output", "exit 0", ogg, valid, InvalidAudio},
		{"empty output", "touch \"$last\"", ogg, valid, InvalidAudio},
		{"oversize output", "head -c 1024 /dev/zero > \"$last\"", ogg, valid, LimitExceeded},
		{"wrong codec", "printf audio > \"$last\"", ogg, strings.ReplaceAll(valid, `"codec_name":"mp3"`, `"codec_name":"aac"`), InvalidAudio},
		{"wrong container", "printf audio > \"$last\"", ogg, ogg, InvalidAudio},
		{"video input", "exit 0", strings.ReplaceAll(ogg, `"codec_type":"audio"`, `"codec_type":"video"`), valid, InvalidAudio},
		{"bad probe", "exit 0", `not json`, valid, InvalidAudio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := "for last do :; done\ncase \"$last\" in */converted/*) printf '%s' '" + tc.outputProbe + "';; *) printf '%s' '" + tc.sourceProbe + "';; esac"
			paths := tools(t, "exit 1", probe)
			marker := filepath.Join(t.TempDir(), "converted")
			paths.FFmpeg = script(t, t.TempDir(), "ffmpeg", "if [ \"$1\" = '-version' ]; then echo 'ffmpeg version 7.1'; exit 0; fi\nfor last do :; done\ntouch '"+marker+"'\n"+tc.convert)
			d, source := fixtureDownload(t, paths, "ogg", []byte("input"))
			d.MaxBytes = 100
			if _, err := d.Fetch(context.Background(), source); !IsCode(err, tc.want) || strings.Contains(err.Error(), "secret-input-path") {
				t.Fatalf("wrong failure: %v", err)
			}
			if tc.name == "video input" || tc.name == "bad probe" {
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("converter ran on invalid input")
				}
			}
			assertNoStaging(t, d.StageParent)
		})
	}
}

func TestMP3ConversionCancellation(t *testing.T) {
	paths := tools(t, "exit 1", `echo '{"format":{"format_name":"ogg"},"streams":[{"codec_type":"audio","codec_name":"vorbis","channels":2}]}'`)
	marker := filepath.Join(t.TempDir(), "child-finished")
	started := filepath.Join(t.TempDir(), "started")
	paths.FFmpeg = script(t, t.TempDir(), "ffmpeg", "if [ \"$1\" = '-version' ]; then echo 'ffmpeg version 7.1'; exit 0; fi\ntouch '"+started+"'\n(sleep 0.5; touch '"+marker+"') &\nsleep 5")
	d, source := fixtureDownload(t, paths, "ogg", []byte("input"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := d.Fetch(ctx, source); result <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conversion did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("conversion did not stop")
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conversion subprocess survived cancellation")
	}
	assertNoStaging(t, d.StageParent)
}

func TestMP3PreflightAvoidsDownloadsWithoutConverter(t *testing.T) {
	paths := tools(t, "exit 1", "exit 1")
	paths.FFmpeg = "/nonexistent/ffmpeg"
	d, source := fixtureDownload(t, paths, "ogg", nil)
	d.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("contacted source with missing converter")
		return nil, errors.New("unexpected network")
	})
	if _, err := d.Preflight(context.Background(), source); !IsCode(err, MissingTool) {
		t.Fatalf("conversion preflight: %v", err)
	}
	if _, err := d.Fetch(context.Background(), source); !IsCode(err, MissingTool) {
		t.Fatalf("conversion fetch: %v", err)
	}
	assertNoStaging(t, d.StageParent)
}
