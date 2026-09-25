package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJavaScriptRuntimePreflightAndArguments(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args")
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: tools(t, "printf '%s\\n' \"$@\" > '"+argsPath+"'; printf audio > theme.mp3; exit 101", `echo '{"format":{"format_name":"mp3"},"streams":[{"codec_type":"audio","codec_name":"mp3","channels":2}]}'`), StageParent: t.TempDir()}
	v, err := d.Preflight(context.Background(), Source{Format: "mp3", Extract: true})
	if err != nil || v.JSRuntime != "node 22.0.0" {
		t.Fatalf("runtime preflight: %+v %v", v, err)
	}
	audio, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Close()
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []string{"--js-runtimes\n" + d.Tools.JSRuntime + "\n", "--sleep-requests\n3\n", "--sleep-interval\n5\n", "--max-sleep-interval\n10\n", "--concurrent-fragments\n1\n", "--retries\n0\n", "--fragment-retries\n0\n", "--extractor-retries\n0\n", "--no-remote-components\n"} {
		if !strings.Contains(string(args), pair) {
			t.Fatalf("missing %q in %s", pair, args)
		}
	}
	for _, spec := range []string{"node:" + script(t, t.TempDir(), "old-node", "echo v20.19.0"), "deno:" + script(t, t.TempDir(), "old-deno", "echo 'deno 2.2.0'"), "node:/nonexistent/runtime", "bun"} {
		d.Tools.JSRuntime = spec
		if _, err := d.Preflight(context.Background(), Source{Format: "mp3", Extract: true}); !IsCode(err, MissingTool) {
			t.Fatalf("accepted unsupported runtime %s: %v", spec, err)
		}
		// Direct audio never acquires a JavaScript or extraction dependency.
		if _, err := d.Preflight(context.Background(), Source{Format: "mp3"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestYouTubeCooldownSurvivesNewDownloaderAndIncludesSearch(t *testing.T) {
	limiter := newYouTubeLimiter(0)
	tool := tools(t, "echo 'ERROR: HTTP Error 429: Too Many Requests' >&2; exit 1", "exit 1")
	d := &Downloader{youtube: limiter, Tools: tool, StageParent: t.TempDir()}
	_, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
	if !IsCode(err, RateLimited) {
		t.Fatalf("throttling misclassified: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "network")
	other := &Downloader{youtube: limiter, Tools: tools(t, "touch '"+marker+"'; echo '{\"entries\":[]}'", "exit 1")}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := other.Search(ctx, SearchOptions{Title: "Example"}); !IsCode(err, Transient) {
		t.Fatalf("new downloader bypassed cooldown: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("search contacted YouTube during cooldown: %v", err)
	}
	if (&Downloader{}).youtubeLimit() != (&Downloader{}).youtubeLimit() {
		t.Fatal("default instances do not share limiter")
	}
}

func TestYouTubeGateSerializesAndPacesJobs(t *testing.T) {
	limiter := newYouTubeLimiter(40 * time.Millisecond)
	release, err := limiter.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := limiter.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent job acquired gate: %v", err)
	}
	release(nil)
	start := time.Now()
	done, err := limiter.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done(nil)
	if time.Since(start) < 35*time.Millisecond {
		t.Fatal("no gap between jobs")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := limiter.acquire(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled job acquired gate: %v", err)
	}
}

func TestExtractorFailuresAreActionableAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		message string
		code    Code
	}{
		{"ERROR HTTP Error 429 https://secret.example/?token=secret", RateLimited},
		{"ERROR This content isn't available, try again later", RateLimited},
		{"ERROR Sign in to confirm you're not a bot", AuthenticationRequired},
		{"WARNING challenge solving failed; install yt-dlp-ejs", MissingTool},
		{"ERROR Unable to download API page: HTTP Error 503", Transient},
		{"ERROR Video unavailable /private/path token=secret", UnavailableMedia},
	} {
		err := extractorError([]byte(tc.message))
		if !IsCode(err, tc.code) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/private/") {
			t.Fatalf("classification: %v", err)
		}
	}
}
