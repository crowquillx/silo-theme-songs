package pluginapp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/engine"
)

func audioName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp3", ".m4a", ".m4b", ".flac", ".ogg", ".opus", ".wav", ".aac":
		return true
	}
	return false
}
func existingAudio(d destinations.Destination, records []engine.Record) string {
	entries, e := os.ReadDir(d.LocalPath)
	if e != nil {
		return "cannot inspect existing audio"
	}
	for _, f := range entries {
		if strings.EqualFold(strings.TrimSuffix(f.Name(), filepath.Ext(f.Name())), "theme") && audioName(f.Name()) {
			return "existing root theme is preserved"
		}
	}
	entries, e = os.ReadDir(filepath.Join(d.LocalPath, "theme-music"))
	if errors.Is(e, os.ErrNotExist) {
		return ""
	}
	if e != nil {
		return "cannot inspect theme-music"
	}
	known := map[string]bool{}
	for _, r := range records {
		if r.LocalPath == d.LocalPath && r.Status != engine.Failed {
			known[r.Filename] = true
		}
	}
	for _, f := range entries {
		if audioName(f.Name()) && !known[f.Name()] {
			return "unowned theme audio is preserved"
		}
	}
	return ""
}
