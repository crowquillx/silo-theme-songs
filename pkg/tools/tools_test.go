package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

func TestEmbeddedKeyVerifiesARealRelease(t *testing.T) {
	sums, _ := os.ReadFile("testdata/SHA2-256SUMS")
	sig, _ := os.ReadFile("testdata/SHA2-256SUMS.sig")
	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(ytdlpSigningKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openpgp.CheckDetachedSignature(ring, bytes.NewReader(sums), bytes.NewReader(sig), nil); err != nil {
		t.Fatalf("yt-dlp 2026.10.21 signature: %v", err)
	}
	if fp := fmt.Sprintf("%X", ring[0].PrimaryKey.Fingerprint); fp != "AC0CBBE6848D6A873464AF4E57CF65933B5A7581" {
		t.Fatalf("unexpected signing key %s", fp)
	}
	tampered := bytes.Replace(sums, []byte("yt-dlp_linux.zip"), []byte("yt-dlp_linux.zap"), 1)
	if _, err := openpgp.CheckDetachedSignature(ring, bytes.NewReader(tampered), bytes.NewReader(sig), nil); err == nil {
		t.Fatal("accepted tampered checksums")
	}
}

type release struct {
	version string
	files   map[string][]byte
	sig     []byte
}

type fixture struct {
	t        *testing.T
	server   *httptest.Server
	signer   *openpgp.Entity
	key      []byte
	latest   atomic.Pointer[release]
	requests atomic.Int32
	down     atomic.Bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t}
	signer, err := openpgp.NewEntity("fixture", "", "fixture@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.signer = signer
	var key bytes.Buffer
	w, _ := armor.Encode(&key, openpgp.PublicKeyType, nil)
	signer.Serialize(w)
	w.Close()
	f.key = key.Bytes()
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		rel := f.latest.Load()
		if strings.HasPrefix(r.URL.Path, "/latest/") {
			http.Redirect(w, r, "/download/"+rel.version+"/"+strings.TrimPrefix(r.URL.Path, "/latest/"), http.StatusFound)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/download/"+rel.version+"/")
		if name == "SHA2-256SUMS.sig" && rel.sig != nil {
			w.Write(rel.sig)
			return
		}
		b, ok := rel.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// publish serves a yt-dlp release whose executable prints version.
func (f *fixture) publish(version string, archive []byte) {
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  yt-dlp_linux.zip\n")
	var sig bytes.Buffer
	if err := openpgp.DetachSign(&sig, f.signer, bytes.NewReader(sums), nil); err != nil {
		f.t.Fatal(err)
	}
	f.latest.Store(&release{version: version, files: map[string][]byte{"SHA2-256SUMS": sums, "yt-dlp_linux.zip": archive}, sig: sig.Bytes()})
}

func zipOf(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, body := range entries {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0755)
		if strings.HasPrefix(body, "->") {
			h.SetMode(os.ModeSymlink | 0777)
			body = strings.TrimPrefix(body, "->")
		}
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	z.Close()
	return b.Bytes()
}

func ytdlpZip(t *testing.T, version string) []byte {
	return zipOf(t, map[string]string{"yt-dlp_linux": "#!/bin/sh\necho " + version + "\n", "_internal/lib.so": "library"})
}

func (f *fixture) manager(dir string) *Manager {
	return &Manager{Dir: dir, Client: f.server.Client(), ytdlpBase: f.server.URL + "/latest/", denoBase: f.server.URL + "/deno/", key: f.key, arch: "amd64"}
}

func run(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command(path).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestYTDLPInstallsChecksDailyAndUpdates(t *testing.T) {
	f := newFixture(t)
	f.publish("2026.10.21", ytdlpZip(t, "2026.10.21"))
	dir := t.TempDir()
	m := f.manager(dir)
	ctx := context.Background()
	path, err := m.YTDLP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, path); got != "2026.10.21" {
		t.Fatalf("installed %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "_internal", "lib.so")); err != nil {
		t.Fatal("onedir support files missing")
	}
	before := f.requests.Load()
	f.publish("2026.11.02", ytdlpZip(t, "2026.11.02"))
	if again, err := m.YTDLP(ctx); err != nil || again != path || f.requests.Load() != before {
		t.Fatalf("checked GitHub again within a day: %v", err)
	}
	// A stale mark waits an hour after the last check.
	m.MarkStale()
	if _, err := m.YTDLP(ctx); err != nil || f.requests.Load() != before {
		t.Fatal("stale check ignored the one-hour minimum")
	}
	st := m.load()
	st.CheckedAt = time.Now().Add(-2 * time.Hour)
	m.save(st)
	updated, err := m.YTDLP(ctx)
	if err != nil || run(t, updated) != "2026.11.02" {
		t.Fatalf("stale copy not updated: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("old release not pruned")
	}
	// An installed copy keeps working while the release host is down.
	f.down.Store(true)
	st = m.load()
	st.CheckedAt = time.Time{}
	m.save(st)
	if kept, err := m.YTDLP(ctx); err != nil || kept != updated {
		t.Fatalf("lost installed copy during outage: %v", err)
	}
	if m.load().FailedAt.IsZero() {
		t.Fatal("failed check not recorded")
	}
}

func TestYTDLPRejectsUnverifiedReleases(t *testing.T) {
	f := newFixture(t)
	other, _ := openpgp.NewEntity("other", "", "other@example.org", nil)
	cases := map[string]func(){
		"foreign signature": func() {
			f.publish("2026.10.21", ytdlpZip(t, "2026.10.21"))
			rel := f.latest.Load()
			var sig bytes.Buffer
			openpgp.DetachSign(&sig, other, bytes.NewReader(rel.files["SHA2-256SUMS"]), nil)
			rel.sig = sig.Bytes()
		},
		"checksum mismatch": func() {
			f.publish("2026.10.21", ytdlpZip(t, "2026.10.21"))
			f.latest.Load().files["yt-dlp_linux.zip"] = ytdlpZip(t, "tampered")
		},
		"symlink entry": func() {
			f.publish("2026.10.21", zipOf(t, map[string]string{"yt-dlp_linux": "->/bin/sh"}))
		},
		"escaping entry": func() {
			f.publish("2026.10.21", zipOf(t, map[string]string{"../yt-dlp_linux": "#!/bin/sh\n"}))
		},
		"bad version": func() {
			f.publish("latest", ytdlpZip(t, "latest"))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			setup()
			dir := t.TempDir()
			if path, err := f.manager(dir).YTDLP(context.Background()); err == nil {
				t.Fatalf("installed %s", path)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "yt-dlp_linux")); err == nil {
				t.Fatal("archive escaped the tools directory")
			}
		})
	}
}

func TestDenoIsPinned(t *testing.T) {
	f := newFixture(t)
	archive := zipOf(t, map[string]string{"deno": "#!/bin/sh\necho deno " + DenoVersion + "\n"})
	f.publish("2026.10.21", ytdlpZip(t, "2026.10.21"))
	mux := http.NewServeMux()
	mux.HandleFunc("/deno/deno-x86_64-unknown-linux-gnu.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	f.server.Config.Handler = mux
	m := f.manager(t.TempDir())
	m.denoSum = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := m.Deno(context.Background()); err == nil {
		t.Fatal("installed Deno with a wrong checksum")
	}
	sum := sha256.Sum256(archive)
	m.denoSum = hex.EncodeToString(sum[:])
	path, err := m.Deno(context.Background())
	if err != nil || run(t, path) != "deno "+DenoVersion {
		t.Fatalf("Deno install: %v", err)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { t.Error("downloaded Deno again") })
	if again, err := m.Deno(context.Background()); err != nil || again != path {
		t.Fatal(err)
	}
}
