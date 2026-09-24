package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const ownerLock = ".silo-theme-download.lock"
const ownerMarker = ".silo-theme-download-owner.json"

type marker struct {
	Protocol int    `json:"protocol"`
	Provider string `json:"provider"`
	OwnerID  string `json:"owner_id"`
}

func lockOwner(ctx context.Context, localPath string) (*os.Root, func(), error) {
	if err := destinationsCheck(localPath); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(localPath)
	if err != nil {
		return nil, nil, err
	}
	if info, err := root.Lstat(ownerLock); err == nil && !info.Mode().IsRegular() {
		root.Close()
		return nil, nil, errors.New("invalid owner lock file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		root.Close()
		return nil, nil, err
	}
	f, err := root.OpenFile(ownerLock, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			root.Close()
			return nil, nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			root.Close()
			return nil, nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return root, func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close(); root.Close() }, nil
}

func destinationsCheck(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("invalid local owner path")
	}
	cur := string(filepath.Separator)
	for _, p := range strings.Split(strings.TrimPrefix(path, cur), cur) {
		cur = filepath.Join(cur, p)
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in owner path")
		}
	}
	return nil
}

func readMarker(root *os.Root) (marker, bool, error) {
	info, err := root.Lstat(ownerMarker)
	if errors.Is(err, os.ErrNotExist) {
		return marker{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return marker{}, false, errors.New("invalid owner marker")
	}
	data, err := root.ReadFile(ownerMarker)
	if err != nil {
		return marker{}, false, err
	}
	var m marker
	if json.Unmarshal(data, &m) != nil || m.Protocol != protocol || m.Provider == "" || m.OwnerID == "" {
		return marker{}, false, errors.New("invalid owner marker")
	}
	return m, true, nil
}

func checkMarker(root *os.Root, provider, ownerID string) error {
	m, exists, err := readMarker(root)
	if err != nil {
		return err
	}
	if exists && (m.Provider != provider || m.OwnerID != ownerID) {
		return errors.New("owner claimed by another provider or catalog owner")
	}
	return nil
}

func requireMarker(root *os.Root, provider, ownerID string) error {
	m, exists, err := readMarker(root)
	if err != nil {
		return err
	}
	if !exists || m.Provider != provider || m.OwnerID != ownerID {
		return errors.New("managed owner marker missing or changed")
	}
	return nil
}

func writeMarker(root *os.Root, provider, ownerID string) error {
	if err := checkMarker(root, provider, ownerID); err != nil {
		return err
	}
	if _, exists, _ := readMarker(root); exists {
		return nil
	}
	data, _ := json.Marshal(marker{Protocol: protocol, Provider: provider, OwnerID: ownerID})
	name, err := randomName(".silo-theme-owner-")
	if err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := root.Link(name, ownerMarker); err != nil {
		return err
	}
	return syncDir(root)
}

func randomName(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func themeRoot(owner *os.Root) (*os.Root, error) {
	info, err := owner.Lstat("theme-music")
	if errors.Is(err, os.ErrNotExist) {
		if err := owner.Mkdir("theme-music", 0755); err != nil {
			return nil, err
		}
		if err := syncDir(owner); err != nil {
			return nil, err
		}
		info, err = owner.Lstat("theme-music")
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe theme-music directory")
	}
	return owner.OpenRoot("theme-music")
}

func checkUnownedAudio(owner, theme *os.Root, owned map[string]Record) error {
	rootDir, err := owner.Open(".")
	if err != nil {
		return err
	}
	names, err := rootDir.Readdirnames(-1)
	rootDir.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(strings.ToLower(name), "theme.") {
			return errors.New("existing root theme requires operator review")
		}
	}
	f, err := theme.Open(".")
	if err != nil {
		return err
	}
	names, err = f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := owned[name]; ok {
			continue
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".mp3", ".m4a", ".m4b", ".flac", ".ogg", ".opus", ".wav", ".aac":
			return errors.New("existing theme audio requires operator review")
		}
		if strings.HasPrefix(strings.ToLower(name), "theme.") {
			return errors.New("existing theme audio requires operator review")
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func copyAndLink(ctx context.Context, theme *os.Root, filename, stagedPath, expectedHash string, expectedSize int64) error {
	if _, err := theme.Lstat(filename); err == nil {
		return errors.New("final theme already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	name, err := randomName(".silo-theme-staging-")
	if err != nil {
		return err
	}
	src, err := os.Open(stagedPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := theme.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer theme.Remove(name)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(contextReader{ctx, src}, maxAudioBytes+1))
	if err == nil {
		err = dst.Sync()
	}
	closeErr := dst.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if n != expectedSize || n > maxAudioBytes || hex.EncodeToString(h.Sum(nil)) != expectedHash {
		return errors.New("staged audio changed before publication")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := theme.Link(name, filename); err != nil {
		return fmt.Errorf("no-clobber publication failed: %w", err)
	}
	return nil
}
