package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
)

func fixture(t *testing.T) (destinations.Destination, string, string) {
	t.Helper()
	base := t.TempDir()
	owner := filepath.Join(base, "media", "Example")
	if err := os.MkdirAll(owner, 0755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(base, "staged.audio")
	if err := os.WriteFile(stage, []byte("fixture audio"), 0600); err != nil {
		t.Fatal(err)
	}
	d := destinations.Destination{LibraryID: "lib", ItemID: "item", OwnerID: "item", LocalPath: owner, ServerPath: "/srv/media/Example", Fingerprint: "proof"}
	return d, stage, filepath.Join(base, "state")
}

func publish(t *testing.T, e *Engine, d destinations.Destination, stage string) Record {
	t.Helper()
	r, err := e.Publish(context.Background(), d, "tmdb:123", "general-themerrdb-123-title.mp3", stage, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPublishAndExactDiscovery(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := publish(t, e, d, stage)
	if r.Status != Published || r.Destination.LocalPath != d.LocalPath || r.Checksum == "" {
		t.Fatalf("record=%+v", r)
	}
	if _, err := os.Stat(filepath.Join(d.LocalPath, "theme-music", r.Filename)); err != nil {
		t.Fatal(err)
	}
	attempt, err := e.MarkAttempt(d, r.Filename, true)
	if err != nil || attempt.Attempts != 1 || attempt.ScanRequests != 1 || attempt.LastAttempt.IsZero() || attempt.Status != Published {
		t.Fatalf("attempt=%+v err=%v", attempt, err)
	}
	if _, err := e.MarkDiscovered(d, r.Filename, "wrong-owner", strings.TrimSuffix(r.Filename, ".mp3")); err == nil {
		t.Fatal("wrong owner accepted")
	}
	if _, err := e.MarkDiscovered(d, r.Filename, d.OwnerID, "wrong title"); err == nil {
		t.Fatal("wrong title accepted")
	}
	observedTitle := strings.TrimSuffix(r.Filename, ".mp3")
	discovered, err := e.MarkDiscovered(d, r.Filename, d.OwnerID, observedTitle)
	if err != nil || discovered.Status != Discovered {
		t.Fatalf("discovery=%+v err=%v", discovered, err)
	}
	if _, err := e.MarkAttempt(d, r.Filename, false); err == nil {
		t.Fatal("discovered theme counted another attempt")
	}
	if _, err := e.Publish(context.Background(), d, "tmdb:123", r.Filename, stage, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}
}

// Themes and theme-music get the default modes reduced by the umask, as
// Jellyfin's theme plugins write them, so other media readers can read them.
func TestPublishedThemeFollowsUmask(t *testing.T) {
	umask := syscall.Umask(0o027)
	defer syscall.Umask(umask)
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := publish(t, e, d, stage)
	for path, want := range map[string]os.FileMode{
		filepath.Join(d.LocalPath, "theme-music"):             0o750,
		filepath.Join(d.LocalPath, "theme-music", r.Filename): 0o640,
		filepath.Join(d.LocalPath, ownerMarker):               0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode %o, want %o", filepath.Base(path), got, want)
		}
	}
}

func TestOwnerProtectionAndNoClobber(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := Open(stateDir, "general"); err == nil {
		t.Fatal("duplicate state open")
	}
	if err := os.Mkdir(filepath.Join(d.LocalPath, "theme-music"), 0755); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(d.LocalPath, "theme-music", "MANUAL.MP3")
	if err := os.WriteFile(manual, []byte("manual"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Publish(context.Background(), d, "tmdb:123", "general-themerrdb-123-title.mp3", stage, func(context.Context) error { return nil }); err == nil {
		t.Fatal("manual theme overwritten")
	}
	if data, _ := os.ReadFile(manual); string(data) != "manual" {
		t.Fatal("manual theme changed")
	}
	if err := os.Remove(manual); err != nil {
		t.Fatal(err)
	}
	r := publish(t, e, d, stage)
	other, err := Open(filepath.Join(filepath.Dir(stateDir), "other-state"), "animethemes")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Publish(context.Background(), d, "anime:1", "animethemes-1-op1.ogg", stage, func(context.Context) error { return nil }); err == nil {
		t.Fatal("cross-provider claim accepted")
	}
	final := filepath.Join(d.LocalPath, "theme-music", r.Filename)
	if err := os.WriteFile(final, []byte("user edit"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = e.MarkDiscovered(d, r.Filename, d.OwnerID, strings.TrimSuffix(r.Filename, ".mp3"))
	if err == nil || e.Records()[0].Status != Protected {
		t.Fatalf("edited file not protected: %v %+v", err, e.Records())
	}
	if data, _ := os.ReadFile(final); string(data) != "user edit" {
		t.Fatal("edited file overwritten")
	}
}

func TestPendingCrashRecoveryAndStaleFreeze(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	r := publish(t, e, d, stage)
	e.mu.Lock()
	e.state.Records[0].Status = Pending
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	e, err = Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	count, err := e.Recover(context.Background())
	if err != nil || count != 1 || e.Records()[0].Status != Published {
		t.Fatalf("recovery=%d %v %+v", count, err, e.Records())
	}
	if err := e.MarkStale(d); err != nil {
		t.Fatal(err)
	}
	if e.Records()[0].Status != Stale {
		t.Fatal("fallback was not frozen")
	}
	if _, err := e.Publish(context.Background(), d, r.SourceID, r.Filename, stage, func(context.Context) error { return nil }); err == nil {
		t.Fatal("stale owner accepted publication")
	}
}

func TestRestoreOnlyExactVerifiedStaleDestination(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := publish(t, e, d, stage)
	if _, err := e.MarkDiscovered(d, r.Filename, d.OwnerID, strings.TrimSuffix(r.Filename, ".mp3")); err != nil {
		t.Fatal(err)
	}
	if err := e.MarkStale(d); err != nil {
		t.Fatal(err)
	}
	changed := d
	changed.Fingerprint = "different proof"
	if n, err := e.Restore(context.Background(), changed); err != nil || n != 0 {
		t.Fatalf("changed proof recovered stale: n=%d err=%v", n, err)
	}
	if e.Records()[0].Status != Stale {
		t.Fatal("stale record changed without exact proof")
	}
	if n, err := e.Restore(context.Background(), d); err != nil || n != 1 {
		t.Fatalf("exact proof failed recovery: n=%d err=%v", n, err)
	}
	restored := e.Records()[0]
	if restored.Status != Published || restored.Attempts != 0 || !restored.LastAttempt.IsZero() {
		t.Fatalf("stale recovery record=%+v", restored)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Records()[0].Status; got != Published {
		t.Fatalf("recovery not durable: %s", got)
	}
}

func TestRestoreNeverUnfreezesProtectedFile(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := publish(t, e, d, stage)
	if err := e.MarkStale(d); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(d.LocalPath, "theme-music", r.Filename)
	if err := os.WriteFile(final, []byte("user changed it"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := e.Restore(context.Background(), d); err != nil || n != 0 {
		t.Fatalf("changed audio recovered: n=%d err=%v", n, err)
	}
	if got := e.Records()[0].Status; got != Protected {
		t.Fatalf("changed audio status=%s", got)
	}
	if err := os.WriteFile(final, []byte("fixture audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := e.Restore(context.Background(), d); err != nil || n != 0 || e.Records()[0].Status != Protected {
		t.Fatalf("protected record unfrozen: n=%d err=%v", n, err)
	}
}

func TestRestoreRequiresOwnerMarker(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	publish(t, e, d, stage)
	if err := e.MarkStale(d); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d.LocalPath, ownerMarker)); err != nil {
		t.Fatal(err)
	}
	if n, err := e.Restore(context.Background(), d); err != nil || n != 0 {
		t.Fatalf("markerless recovery: n=%d err=%v", n, err)
	}
	if e.Records()[0].Status != Protected {
		t.Fatal("markerless stale file was restored")
	}
}

func TestMultipleDiscoveredThemes(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "animethemes")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	first := publish(t, e, d, stage)
	_, err = e.Publish(context.Background(), d, "anime:2", "animethemes-2-ed1.ogg", stage, func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("second theme published before first discovery")
	}
	if _, err := e.MarkDiscovered(d, first.Filename, d.OwnerID, strings.TrimSuffix(first.Filename, ".mp3")); err != nil {
		t.Fatal(err)
	}
	second, err := e.Publish(context.Background(), d, "anime:2", "animethemes-2-ed1.ogg", stage, func(context.Context) error { return nil })
	if err != nil || second.Status != Published {
		t.Fatalf("second theme: %+v %v", second, err)
	}
	_, err = e.Publish(context.Background(), d, "anime:3", "animethemes-3-op2.ogg", stage, func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("third theme published before second discovery")
	}
	if _, err := e.MarkDiscovered(d, second.Filename, d.OwnerID, strings.TrimSuffix(second.Filename, ".ogg")); err != nil {
		t.Fatal(err)
	}
	if checked, err := e.Check(context.Background(), d); err != nil || len(checked) != 2 {
		t.Fatalf("check=%+v err=%v", checked, err)
	}
	third, err := e.Publish(context.Background(), d, "anime:3", "animethemes-3-op2.ogg", stage, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDiscovered(d, third.Filename, d.OwnerID, strings.TrimSuffix(third.Filename, ".ogg")); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(d.LocalPath, "theme-music", "manual.flac")
	if err := os.WriteFile(manual, []byte("user audio"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Publish(context.Background(), d, "anime:4", "animethemes-4-op3.ogg", stage, func(context.Context) error { return nil }); err == nil {
		t.Fatal("unowned audio accepted")
	}
}

func TestDiscoveredChecksumCheckedAgain(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := publish(t, e, d, stage)
	title := strings.TrimSuffix(r.Filename, ".mp3")
	if _, err := e.MarkDiscovered(d, r.Filename, d.OwnerID, title); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.LocalPath, "theme-music", r.Filename), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDiscovered(d, r.Filename, d.OwnerID, title); err == nil {
		t.Fatal("second discovery skipped checksum")
	}
	if e.Records()[0].Status != Protected {
		t.Fatal("manual edit was not protected")
	}
}

func TestRecoverMissingFinalAllowsRetry(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := Record{Destination: d, Provider: "general", LibraryID: d.LibraryID, OwnerID: d.OwnerID, LocalPath: d.LocalPath, ServerPath: d.ServerPath, Fingerprint: d.Fingerprint, SourceID: "tmdb:123", Filename: "general-themerrdb-123-title.mp3", Checksum: "old", Status: Pending}
	e.mu.Lock()
	e.state.Records = append(e.state.Records, r)
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	count, err := e.Recover(context.Background())
	if err != nil || count != 0 || e.Records()[0].Status != Failed {
		t.Fatalf("recovery=%d %v %+v", count, err, e.Records())
	}
	newRecord := publish(t, e, d, stage)
	if newRecord.Status != Published || len(e.Records()) != 1 {
		t.Fatalf("retry=%+v %+v", newRecord, e.Records())
	}
}

func TestCheckRequiresOwnerMarker(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := publish(t, e, d, stage)
	if _, err := e.MarkDiscovered(d, r.Filename, d.OwnerID, strings.TrimSuffix(r.Filename, ".mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d.LocalPath, ownerMarker)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Check(context.Background(), d); err == nil {
		t.Fatal("missing owner marker accepted")
	}
}

func TestPublishRevalidationAndLockCancellation(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	_, err = e.Publish(context.Background(), d, "tmdb:123", "general-themerrdb-123-title.mp3", stage, func(context.Context) error { return errors.New("media moved") })
	if err == nil || len(e.Records()) != 0 {
		t.Fatalf("stale preview published: %v", err)
	}
	locked, unlock, err := lockOwner(context.Background(), d.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = e.Publish(ctx, d, "tmdb:123", "general-themerrdb-123-title.mp3", stage, func(context.Context) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation: %v", err)
	}
	unlock()
}

// Versions before 0.2.0 kept an Autoscan journal in state.json.
func TestLegacyJournalStateLoads(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	r := publish(t, e, d, stage)
	e.Close()
	data, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(data), `"records":`, `"next_seq":4,"pruned_through":1,"events":[{"seq":4,"library_id":"lib","server_path":"/srv/media/Example","created":"2026-10-01T00:00:00Z"}],"generations":{"g":{"baseline":1,"ready":true}},"issued":{},"repair":[],"records":`, 1)
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	e, err = Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if records := e.Records(); len(records) != 1 || records[0].Filename != r.Filename || records[0].Status != Published {
		t.Fatalf("legacy records=%+v", records)
	}
	if _, err := e.MarkAttempt(d, r.Filename, false); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(stateDir, "state.json"))
	if strings.Contains(string(data), "generations") || strings.Contains(string(data), "events") {
		t.Fatalf("legacy journal kept after save: %s", data)
	}
}

func TestOversizeStateRejectedBeforeDecode(t *testing.T) {
	stateDir := t.TempDir()
	f, err := os.Create(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((16 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if e, err := Open(stateDir, "general"); err == nil {
		e.Close()
		t.Fatal("oversize state accepted")
	}
}
