package pluginapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/crowquillx/silo-theme-songs/pkg/engine"
)

// AnimeThemes was a separate plugin before 0.2.0. Its themes stay managed:
// Theme Songs copies its state the first time it handles anime, and leaves
// anime to it while it is still installed and enabled.
const legacyAnimePlugin = "dev.crowquillx.anime-themes"

// legacyAnimeFiles are the AnimeThemes plugin's state and mapping caches.
var legacyAnimeFiles = []string{"state.json", "anibridge-v3.json", "anime-lists.xml"}

// legacyAnimeEnabled reports whether the AnimeThemes plugin is installed and
// enabled. It is false when the host cannot say.
func legacyAnimeEnabled(ctx context.Context) bool {
	host := sdkruntime.Host()
	if host == nil {
		return false
	}
	plugins, err := host.ListInstalledPlugins(ctx)
	if err != nil {
		return false
	}
	for _, p := range plugins {
		if p.GetPluginId() == legacyAnimePlugin && p.GetEnabled() {
			return true
		}
	}
	return false
}

// migrateLegacyAnime copies the AnimeThemes plugin's default state directory
// into dir when dir has no state yet. It reports whether it copied anything.
func migrateLegacyAnime(cacheDir, dir string) (bool, error) {
	if cacheDir == "" {
		return false, nil
	}
	if _, err := os.Lstat(filepath.Join(dir, "state.json")); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	legacy := filepath.Join(cacheDir, "plugin-state", legacyAnimePlugin)
	if _, err := os.Lstat(filepath.Join(legacy, "state.json")); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	// Opening the old state validates it and takes its lock, so the copy
	// fails while the AnimeThemes plugin still runs.
	old, err := engine.Open(legacy, "animethemes")
	if err != nil {
		return false, fmt.Errorf("AnimeThemes plugin state: %w", err)
	}
	defer old.Close()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	// state.json goes last, so an interrupted copy is retried.
	for i := len(legacyAnimeFiles) - 1; i >= 0; i-- {
		name := legacyAnimeFiles[i]
		if err := copyRegular(filepath.Join(legacy, name), dir, name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("copy AnimeThemes %s: %w", name, err)
		}
	}
	return true, nil
}

func copyRegular(src, dir, name string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return errors.New("not a bounded regular file")
	}
	out, err := os.CreateTemp(dir, ".legacy-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if _, err = io.Copy(out, io.LimitReader(in, 64<<20)); err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(out.Name(), filepath.Join(dir, name))
}
