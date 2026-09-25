package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/ratelimit"
)

type ToolPaths struct {
	YTDLP     string
	FFmpeg    string
	FFprobe   string
	JSRuntime string // deno[:path] or node[:path]; empty discovers a supported runtime
}

type ToolVersions struct {
	YTDLP        string
	FFmpeg       string
	FFprobe      string
	JSRuntime    string
	jsRuntimeArg string
}

// StagedAudio exists only in a private job directory. The caller must call
// Close after its own validated no-clobber publication, including on errors.
type StagedAudio struct {
	Path   string
	Format string
	Size   int64
	dir    string
}

func (s *StagedAudio) Close() error {
	if s == nil || s.dir == "" {
		return nil
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return err
	}
	s.dir = ""
	return nil
}

type Downloader struct {
	Tools              ToolPaths
	StageParent        string
	Client             *http.Client
	AllowedDirectHosts []string
	MaxBytes           int64
	MaxDuration        time.Duration

	mu           sync.Mutex
	failedVideos map[[32]byte]struct{}
	brokenUntil  time.Time
	youtube      *youtubeLimiter // nil uses the process-wide limiter
}

func (d *Downloader) limits() (int64, time.Duration) {
	max := d.MaxBytes
	if max <= 0 {
		max = 80 << 20
	}
	if max > 512<<20 {
		max = 512 << 20
	}
	duration := d.MaxDuration
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	if duration > 20*time.Minute {
		duration = 20 * time.Minute
	}
	return max, duration
}

func (d *Downloader) Preflight(ctx context.Context, extraction bool) (ToolVersions, error) {
	var versions ToolVersions
	var err error
	versions.FFprobe, err = checkTool(ctx, d.Tools.FFprobe, "ffprobe", "-version")
	if err != nil {
		return versions, err
	}
	if !extraction {
		return versions, nil
	}
	versions.YTDLP, err = checkTool(ctx, d.Tools.YTDLP, "yt-dlp", "--version")
	if err != nil {
		return versions, err
	}
	versions.FFmpeg, err = checkTool(ctx, d.Tools.FFmpeg, "ffmpeg", "-version")
	if err != nil {
		return versions, err
	}
	versions.jsRuntimeArg, versions.JSRuntime, err = resolveJSRuntime(ctx, d.Tools.JSRuntime)
	return versions, err
}

func checkTool(ctx context.Context, path, name, arg string) (string, error) {
	if path == "" {
		return "", &Error{MissingTool, name}
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", &Error{MissingTool, name}
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return "", &Error{MissingTool, name}
	}
	timeout, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.Command(resolved, arg)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	var output limitedBuffer
	output.max = 4096
	cmd.Stdout = &output
	cmd.Stderr = &limitedBuffer{max: 1024}
	err = runBounded(timeout, cmd, "", 0)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	out, overflow := output.snapshot()
	if err != nil || overflow {
		return "", &Error{MissingTool, name}
	}
	first := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if name == "yt-dlp" {
		if !regexp.MustCompile(`^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}$`).MatchString(first) || first < "2025.11.12" {
			return "", &Error{MissingTool, name + " version"}
		}
		return first, nil
	}
	fields := strings.Fields(first)
	if name == "node" || name == "deno" {
		version := strings.TrimPrefix(first, "v")
		if name == "deno" && len(fields) == 2 && fields[0] == "deno" {
			version = fields[1]
		}
		m := regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.[0-9]+$`).FindStringSubmatch(version)
		if len(m) != 3 {
			return "", &Error{MissingTool, name + " version"}
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if name == "node" && major < 22 || name == "deno" && (major < 2 || major == 2 && minor < 3) {
			return "", &Error{MissingTool, name + " version"}
		}
		return version, nil
	}
	if len(fields) < 3 || fields[0] != name || fields[1] != "version" {
		return "", &Error{MissingTool, name + " version"}
	}
	m := regexp.MustCompile(`^([0-9]+)\.([0-9]+)`).FindStringSubmatch(fields[2])
	if len(m) != 3 {
		return "", &Error{MissingTool, name + " version"}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 4 || major == 4 && minor < 4 {
		return "", &Error{MissingTool, name + " version"}
	}
	return fields[2], nil
}

func (d *Downloader) Fetch(ctx context.Context, source Source) (_ *StagedAudio, err error) {
	if source.URL == "" {
		return nil, &Error{UnsafeURL, "source"}
	}
	r := &Resolver{AllowedDirectHosts: d.AllowedDirectHosts}
	checked, err := r.source(source.URL, source.Origin, source.ProviderID)
	if err != nil || checked.Extract != source.Extract || checked.Format != source.Format {
		return nil, &Error{UnsafeURL, "source"}
	}
	max, duration := d.limits()
	jobCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	if source.Extract {
		d.mu.Lock()
		blocked := time.Now().Before(d.brokenUntil)
		d.mu.Unlock()
		if blocked {
			return nil, &Error{ExtractorBroken, "circuit open"}
		}
	}
	versions, err := d.Preflight(jobCtx, source.Extract)
	if err != nil {
		return nil, err
	}
	if d.StageParent == "" {
		return nil, &Error{Malformed, "stage parent"}
	}
	dir, err := os.MkdirTemp(d.StageParent, ".silo-theme-provider-")
	if err != nil {
		return nil, &Error{Transient, "staging"}
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	var path string
	if source.Extract {
		path, err = d.extract(jobCtx, dir, source.URL, max, versions.jsRuntimeArg)
		if err != nil {
			if IsCode(err, UnavailableMedia) {
				d.recordFailure(source.URL)
			}
			return nil, err
		}
	} else {
		path, err = d.downloadDirect(jobCtx, dir, source, max)
		if err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && source.Extract {
		return nil, &Error{UnavailableMedia, "extractor produced no audio"}
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, &Error{InvalidAudio, "staged file"}
	}
	if info.Size() > max {
		return nil, &Error{LimitExceeded, "staged size"}
	}
	if err := d.probe(jobCtx, path, source.Format); err != nil {
		return nil, err
	}
	if source.Extract {
		d.mu.Lock()
		d.failedVideos = nil
		d.brokenUntil = time.Time{}
		d.mu.Unlock()
	}
	return &StagedAudio{Path: path, Format: source.Format, Size: info.Size(), dir: dir}, nil
}

func (d *Downloader) recordFailure(rawURL string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failedVideos == nil {
		d.failedVideos = make(map[[32]byte]struct{})
	}
	d.failedVideos[sha256.Sum256([]byte(rawURL))] = struct{}{}
	if len(d.failedVideos) >= 3 {
		d.brokenUntil = time.Now().Add(10 * time.Minute)
	}
}

func (d *Downloader) extract(ctx context.Context, dir, raw string, max int64, jsRuntime string) (_ string, resultErr error) {
	release, err := d.youtubeLimit().acquire(ctx)
	if err != nil {
		return "", err
	}
	defer func() { release(resultErr) }()
	path := filepath.Join(dir, "theme.mp3")
	ffmpegPath, err := exec.LookPath(d.Tools.FFmpeg)
	if err != nil {
		return "", &Error{MissingTool, "ffmpeg"}
	}
	ffmpegPath, err = filepath.Abs(ffmpegPath)
	if err != nil {
		return "", &Error{MissingTool, "ffmpeg"}
	}
	args := []string{"--ignore-config", "--no-config-locations", "--no-plugin-dirs", "--no-playlist", "--use-extractors", "youtube,youtube:tab,end", "--default-search", "error", "--no-cache-dir", "--no-progress", "--no-write-info-json", "--no-write-thumbnail", "--no-write-subs", "--no-write-auto-subs", "--no-mtime", "--socket-timeout", "15", "--max-filesize", strconv.FormatInt(max, 10), "--max-downloads", "1", "--match-filters", "!is_live & duration <= 1200", "-f", "bestaudio/best", "-x", "--audio-format", "mp3", "--audio-quality", "192K", "--ffmpeg-location", ffmpegPath, "-o", filepath.Join(dir, "theme.%(ext)s"), "--", raw}
	args = append(youtubeArgs(), args...)
	args = append([]string{"--no-js-runtimes", "--js-runtimes", jsRuntime}, args...)
	cmd := exec.Command(d.Tools.YTDLP, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + filepath.Dir(ffmpegPath) + ":/usr/bin:/bin", "HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "XDG_CACHE_HOME=" + dir, "TMPDIR=" + dir}
	// --max-downloads 1 returns 101 after a successful download. Accept that
	// stop only here; Fetch still requires a complete, probed audio-only file.
	if err := runBounded(ctx, cmd, dir, max*2+(1<<20), 101); err != nil {
		return "", err
	}
	return path, nil
}

func (d *Downloader) downloadDirect(ctx context.Context, dir string, source Source, max int64) (string, error) {
	path := filepath.Join(dir, "theme."+source.Format)
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copyClient := *client
	copyClient.Transport = ratelimit.Wrap(client.Transport, ratelimit.ExternalInterval)
	if copyClient.Timeout == 0 || copyClient.Timeout > 30*time.Second {
		copyClient.Timeout = 30 * time.Second
	}
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return &Error{UnsafeURL, "redirect limit"}
		}
		u, e := safeURL(req.URL.String())
		if e != nil || !hostAllowed(u.Hostname(), d.AllowedDirectHosts) {
			return &Error{UnsafeURL, "redirect host"}
		}
		return nil
	}
	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
		req.Header.Set("User-Agent", ratelimit.UserAgent)
		req.Header.Set("Accept", "audio/*,application/ogg,application/octet-stream;q=0.8")
		var requestErr error
		resp, requestErr = copyClient.Do(req)
		if IsCode(requestErr, UnsafeURL) {
			return "", &Error{UnsafeURL, "redirect"}
		}
		if ctx.Err() != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return "", ctx.Err()
		}
		if errors.Is(requestErr, ratelimit.ErrDeferred) {
			return "", &Error{Transient, "direct service cooldown; retry later"}
		}
		if requestErr == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if requestErr == nil {
			resp.Body.Close()
		}
		if requestErr == nil && resp.StatusCode != 429 && resp.StatusCode < 500 {
			return "", &Error{UnavailableMedia, "direct HTTP"}
		}
		if attempt == 2 {
			return "", &Error{Transient, "direct HTTP"}
		}
		lastURL := req.URL
		if resp != nil && resp.Request != nil {
			lastURL = resp.Request.URL
		}
		if ratelimit.Waiting(ctx, lastURL) {
			return "", &Error{Transient, "direct service cooldown; retry later"}
		}
		wait := time.Duration(1<<attempt) * time.Second
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
	defer resp.Body.Close()
	if resp.ContentLength > max {
		return "", &Error{LimitExceeded, "direct size"}
	}
	if contentType := strings.ToLower(resp.Header.Get("Content-Type")); strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "html") || strings.Contains(contentType, "json") {
		return "", &Error{InvalidAudio, "direct content type"}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", &Error{Transient, "staging"}
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		return "", &Error{Transient, "direct body"}
	}
	if n > max {
		return "", &Error{LimitExceeded, "direct size"}
	}
	if n == 0 || resp.ContentLength >= 0 && n != resp.ContentLength {
		return "", &Error{InvalidAudio, "direct body"}
	}
	return path, nil
}

func (d *Downloader) probe(ctx context.Context, path, format string) error {
	cmd := exec.Command(d.Tools.FFprobe, "-v", "error", "-protocol_whitelist", "file,pipe", "-max_alloc", "67108864", "-probesize", "10000000", "-analyzeduration", "10000000", "-show_entries", "stream=codec_type:format=format_name", "-of", "json", "--", path)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	var output limitedBuffer
	output.max = 16 << 10
	cmd.Stdout = &output
	cmd.Stderr = &limitedBuffer{max: 1024}
	if err := runBounded(ctx, cmd, "", 0); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{InvalidAudio, "ffprobe"}
	}
	var data struct {
		Format struct {
			Name string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	buf, overflow := output.snapshot()
	if overflow || json.Unmarshal(buf, &data) != nil {
		return &Error{InvalidAudio, "ffprobe output"}
	}
	audio := 0
	for _, s := range data.Streams {
		if s.CodecType == "video" {
			return &Error{InvalidAudio, "video stream"}
		}
		if s.CodecType == "audio" {
			audio++
		}
	}
	if audio == 0 {
		return &Error{InvalidAudio, "no audio stream"}
	}
	expected := map[string]string{"mp3": "mp3", "m4a": "mov,mp4,m4a,3gp,3g2,mj2", "m4b": "mov,mp4,m4a,3gp,3g2,mj2", "ogg": "ogg", "opus": "ogg", "flac": "flac", "wav": "wav", "aac": "aac"}[format]
	if expected == "" || data.Format.Name != expected {
		return &Error{InvalidAudio, "format does not match extension"}
	}
	return nil
}

type limitedBuffer struct {
	mu       sync.Mutex
	buf      []byte
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(b.buf)+n > b.max {
		b.overflow = true
		p = p[:maxInt(0, b.max-len(b.buf))]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}
func (b *limitedBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...), b.overflow
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func runBounded(ctx context.Context, cmd *exec.Cmd, dir string, maxSize int64, acceptedExitCodes ...int) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output limitedBuffer
	output.max = 8 << 10
	if cmd.Stdout == nil {
		cmd.Stdout = &output
	}
	if cmd.Stderr == nil {
		cmd.Stderr = &output
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{MissingTool, "process start"}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, overflow := output.snapshot(); overflow {
				return &Error{LimitExceeded, "process output"}
			}
			if err != nil {
				var exitError *exec.ExitError
				accepted := false
				if errors.As(err, &exitError) {
					for _, code := range acceptedExitCodes {
						accepted = accepted || exitError.ExitCode() == code
					}
				}
				if !accepted {
					output, _ := output.snapshot()
					return extractorError(output)
				}
			}
			if dir != "" && stagedSize(dir) > maxSize {
				return &Error{LimitExceeded, "staging"}
			}
			return nil
		case <-ctx.Done():
			killGroup(cmd.Process.Pid)
			<-done
			return ctx.Err()
		case <-tick.C:
			_, overflow := output.snapshot()
			if overflow || dir != "" && stagedSize(dir) > maxSize {
				killGroup(cmd.Process.Pid)
				<-done
				return &Error{LimitExceeded, "process bounds"}
			}
		}
	}
}

func stagedSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func killGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// Diagnostic returns a bounded, redacted operational state. It never includes
// the source URL or tool output, which can contain URL tokens and local paths.
func Diagnostic(err error, versions ToolVersions) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var e *Error
	if !errors.As(err, &e) {
		return "provider_error"
	}
	return fmt.Sprintf("%s (%s; yt-dlp=%s ffmpeg=%s ffprobe=%s js=%s)", e.Code, e.Op, versions.YTDLP, versions.FFmpeg, versions.FFprobe, versions.JSRuntime)
}
