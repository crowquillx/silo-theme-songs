// Package tools installs verified yt-dlp and Deno releases for hosts that do
// not provide them, such as Silo's Docker image.
package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/crowquillx/silo-theme-songs/pkg/ratelimit"
	"github.com/hashicorp/go-hclog"
)

// ytdlpSigningKey is yt-dlp's release key, fingerprint
// AC0C BBE6 848D 6A87 3464 AF4E 57CF 6593 3B5A 7581.
//
//go:embed yt-dlp-signing-key.asc
var ytdlpSigningKey []byte

// Deno is pinned; update the version and both checksums together.
const DenoVersion = "2.9.8"

var denoSHA256 = map[string]string{
	"amd64": "e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609",
	"arm64": "d49596c20b9788cf8f5d8ddf644103d4bde56ed239753f0bdb92ba9361d51905",
}

const (
	checkInterval = 24 * time.Hour
	retryInterval = time.Hour
	maxArchive    = 256 << 20
	maxExtracted  = 512 << 20
)

var ytdlpVersion = regexp.MustCompile(`^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}(\.[0-9]+)?$`)

// Manager keeps one yt-dlp and one Deno beneath Dir. yt-dlp follows its
// latest signed release and is checked daily, or within an hour after
// MarkStale. Deno is the pinned release above.
type Manager struct {
	Dir    string
	Client *http.Client // nil uses a paced HTTPS client
	Log    hclog.Logger // nil discards

	// Tests override the release sources and platform.
	ytdlpBase string
	denoBase  string
	key       []byte
	arch      string
	denoSum   string

	mu    sync.Mutex
	stale bool
}

type state struct {
	YTDLP     string    `json:"yt_dlp,omitempty"`
	CheckedAt time.Time `json:"yt_dlp_checked_at"`
	FailedAt  time.Time `json:"yt_dlp_failed_at"`
	Deno      string    `json:"deno,omitempty"`
}

// MarkStale requests an early yt-dlp release check, for example after YouTube
// extraction fails in a way a newer yt-dlp may fix.
func (m *Manager) MarkStale() {
	m.mu.Lock()
	m.stale = true
	m.mu.Unlock()
}

// YTDLP returns the managed yt-dlp executable, installing or updating it when
// due. An installed copy keeps working while GitHub is unreachable.
func (m *Manager) YTDLP(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.load()
	installed := st.YTDLP != "" && executable(m.ytdlpPath(st.YTDLP))
	now := time.Now()
	due := !installed || now.Sub(st.CheckedAt) >= checkInterval || (m.stale && now.Sub(st.CheckedAt) >= retryInterval)
	if installed && now.Sub(st.FailedAt) < retryInterval {
		due = false
	}
	if !due {
		return m.ytdlpPath(st.YTDLP), nil
	}
	version, err := m.installYTDLP(ctx, st.YTDLP)
	if err != nil {
		st.FailedAt = now
		m.save(st)
		m.logger().Warn("yt-dlp release check failed", "installed", st.YTDLP, "error", err)
		if installed {
			return m.ytdlpPath(st.YTDLP), nil
		}
		return "", fmt.Errorf("install yt-dlp: %w", err)
	}
	if version != st.YTDLP {
		m.logger().Info("installed yt-dlp", "version", version, "previous", st.YTDLP, "dir", m.Dir)
	}
	m.stale = false
	st.YTDLP, st.CheckedAt, st.FailedAt = version, now, time.Time{}
	if err := m.save(st); err != nil {
		return "", err
	}
	m.prune("yt-dlp", version)
	return m.ytdlpPath(version), nil
}

// Deno returns the managed Deno executable, installing the pinned release.
func (m *Manager) Deno(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.load()
	exe := filepath.Join(m.Dir, "deno", DenoVersion, "deno")
	if st.Deno == DenoVersion && executable(exe) {
		return exe, nil
	}
	if err := m.installDeno(ctx); err != nil {
		m.logger().Warn("Deno install failed", "version", DenoVersion, "error", err)
		return "", fmt.Errorf("install Deno: %w", err)
	}
	m.logger().Info("installed Deno", "version", DenoVersion, "dir", m.Dir)
	st.Deno = DenoVersion
	if err := m.save(st); err != nil {
		return "", err
	}
	m.prune("deno", DenoVersion)
	return exe, nil
}

func (m *Manager) logger() hclog.Logger {
	if m.Log == nil {
		return hclog.NewNullLogger()
	}
	return m.Log
}

func (m *Manager) platform() string {
	if m.arch != "" {
		return m.arch
	}
	return runtime.GOARCH
}

func (m *Manager) ytdlpAsset() (string, error) {
	switch m.platform() {
	case "amd64":
		return "yt-dlp_linux", nil
	case "arm64":
		return "yt-dlp_linux_aarch64", nil
	}
	return "", errors.New("unsupported platform")
}

func (m *Manager) ytdlpPath(version string) string {
	name, _ := m.ytdlpAsset()
	return filepath.Join(m.Dir, "yt-dlp", version, name)
}

func (m *Manager) installYTDLP(ctx context.Context, current string) (string, error) {
	asset, err := m.ytdlpAsset()
	if err != nil {
		return "", err
	}
	base := m.ytdlpBase
	if base == "" {
		base = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/"
	}
	dir, version, err := m.latest(ctx, base+"SHA2-256SUMS")
	if err != nil {
		return "", err
	}
	// Fetch the checksums, signature and archive from that one release.
	sums, err := m.get(ctx, dir+"SHA2-256SUMS", 1<<20)
	if err != nil {
		return "", err
	}
	sig, err := m.get(ctx, dir+"SHA2-256SUMS.sig", 64<<10)
	if err != nil {
		return "", err
	}
	key := m.key
	if key == nil {
		key = ytdlpSigningKey
	}
	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key))
	if err != nil {
		return "", err
	}
	if _, err := openpgp.CheckDetachedSignature(ring, bytes.NewReader(sums), bytes.NewReader(sig), nil); err != nil {
		return "", errors.New("yt-dlp checksums are not signed by the yt-dlp release key")
	}
	if version == current {
		return version, nil
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset+".zip" {
			want = fields[0]
		}
	}
	if want == "" {
		return "", errors.New("signed checksums omit " + asset + ".zip")
	}
	archive, err := m.download(ctx, dir+asset+".zip", want)
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)
	err = m.unpack(archive, filepath.Join(m.Dir, "yt-dlp", version), func(name string) bool { return true })
	if err != nil {
		return "", err
	}
	if !executable(m.ytdlpPath(version)) {
		return "", errors.New("yt-dlp archive has no executable")
	}
	return version, nil
}

func (m *Manager) installDeno(ctx context.Context) error {
	var target string
	switch m.platform() {
	case "amd64":
		target = "x86_64"
	case "arm64":
		target = "aarch64"
	default:
		return errors.New("unsupported platform")
	}
	sum := m.denoSum
	if sum == "" {
		sum = denoSHA256[m.platform()]
	}
	base := m.denoBase
	if base == "" {
		base = "https://github.com/denoland/deno/releases/download/v" + DenoVersion + "/"
	}
	archive, err := m.download(ctx, base+"deno-"+target+"-unknown-linux-gnu.zip", sum)
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	dest := filepath.Join(m.Dir, "deno", DenoVersion)
	if err := m.unpack(archive, dest, func(name string) bool { return name == "deno" }); err != nil {
		return err
	}
	if !executable(filepath.Join(dest, "deno")) {
		return errors.New("Deno archive has no executable")
	}
	return nil
}

func (m *Manager) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{
		Timeout:   10 * time.Minute,
		Transport: ratelimit.Wrap(nil, ratelimit.ExternalInterval),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 || req.URL.Scheme != "https" {
				return errors.New("unsafe tool download redirect")
			}
			return nil
		},
	}
}

func (m *Manager) request(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ratelimit.UserAgent)
	resp, err := m.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("tool download failed")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("tool download returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// latest reads the release that a GitHub latest-download link redirects to,
// .../download/<version>/<file>, without following it to the asset CDN.
func (m *Manager) latest(ctx context.Context, rawURL string) (dir, version string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", ratelimit.UserAgent)
	client := *m.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", errors.New("yt-dlp release check failed")
	}
	resp.Body.Close()
	loc, err := resp.Location()
	if err != nil {
		return "", "", fmt.Errorf("yt-dlp release check returned HTTP %d", resp.StatusCode)
	}
	if loc.Scheme != "https" && loc.Scheme != req.URL.Scheme {
		return "", "", errors.New("unsafe yt-dlp release redirect")
	}
	version = path.Base(path.Dir(loc.Path))
	if !ytdlpVersion.MatchString(version) || path.Base(path.Dir(path.Dir(loc.Path))) != "download" {
		return "", "", errors.New("unrecognized yt-dlp release")
	}
	loc.Path = path.Dir(loc.Path) + "/"
	loc.RawQuery, loc.Fragment = "", ""
	return loc.String(), version, nil
}

// get returns a small response body.
func (m *Manager) get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	resp, err := m.request(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("incomplete tool release metadata")
	}
	return b, nil
}

// download saves an archive beneath Dir and checks its SHA-256.
func (m *Manager) download(ctx context.Context, rawURL, want string) (string, error) {
	if err := os.MkdirAll(m.Dir, 0700); err != nil {
		return "", err
	}
	resp, err := m.request(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(m.Dir, ".download-")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArchive+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > maxArchive {
		err = errors.New("tool archive exceeds 256 MiB")
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = errors.New("tool archive checksum mismatch")
	}
	if err != nil {
		os.Remove(f.Name())
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	return f.Name(), nil
}

// unpack extracts regular files and directories into a fresh directory, then
// renames it into place. Entries cannot leave the directory.
func (m *Manager) unpack(archive, dest string, keep func(string) bool) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return errors.New("invalid tool archive")
	}
	defer z.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".unpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer root.Close()
	var total int64
	for _, f := range z.File {
		name := strings.TrimSuffix(f.Name, "/")
		if name == "" || !keep(name) {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := root.MkdirAll(name, 0755); err != nil {
				return err
			}
			continue
		case !mode.IsRegular():
			return errors.New("tool archive contains a link or special file")
		}
		total += int64(f.UncompressedSize64)
		if total > maxExtracted {
			return errors.New("tool archive expands beyond 512 MiB")
		}
		if dir := path.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0755); err != nil {
				return err
			}
		}
		perm := os.FileMode(0644)
		if mode&0111 != 0 {
			perm = 0755
		}
		if err := extract(root, f, name, perm); err != nil {
			return err
		}
	}
	os.RemoveAll(dest)
	return os.Rename(tmp, dest)
}

func extract(root *os.Root, f *zip.File, name string, perm os.FileMode) error {
	r, err := f.Open()
	if err != nil {
		return errors.New("invalid tool archive")
	}
	defer r.Close()
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, int64(f.UncompressedSize64)+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && uint64(n) != f.UncompressedSize64 {
		err = errors.New("tool archive entry size mismatch")
	}
	return err
}

// prune removes other installed versions of a tool.
func (m *Manager) prune(tool, keep string) {
	entries, _ := os.ReadDir(filepath.Join(m.Dir, tool))
	for _, e := range entries {
		if e.Name() != keep {
			os.RemoveAll(filepath.Join(m.Dir, tool, e.Name()))
		}
	}
}

func (m *Manager) load() state {
	var st state
	b, err := os.ReadFile(filepath.Join(m.Dir, "state.json"))
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (m *Manager) save(st state) error {
	if err := os.MkdirAll(m.Dir, 0700); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	f, err := os.CreateTemp(m.Dir, ".state-")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(m.Dir, "state.json"))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func executable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0
}
