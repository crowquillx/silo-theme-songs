// Package destinations proves ownership only for conservative local layouts.
package destinations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
)

type Mount struct {
	Server string `json:"server"`
	Local  string `json:"local"`
}
type Policy struct {
	Mounts       []Mount           `json:"path_mappings"`
	AllowedRoots []string          `json:"allowed_roots"`
	FlatFallback bool              `json:"single_season_flat_fallback"`
	Overrides    map[string]string `json:"destination_overrides"`
}
type Destination struct {
	LibraryID   string `json:"library_id"`
	ItemID      string `json:"item_id"`
	OwnerID     string `json:"owner_id"`
	Season      int    `json:"season"`
	ServerPath  string `json:"server_path"`
	LocalPath   string `json:"local_path"`
	SeriesRoot  string `json:"series_root"`
	Fingerprint string `json:"fingerprint"`
	Fallback    bool   `json:"fallback"`
}

var seasonFolder = regexp.MustCompile(`(?i)^season[ ._-]*(\d{1,3})$`)

func Within(root, path string) bool {
	r, e := filepath.Rel(root, path)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}
func cleanAbs(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && p != "/" && !strings.ContainsRune(p, 0)
}
func (p Policy) Validate() error {
	if len(p.AllowedRoots) == 0 {
		return errors.New("allowed writable roots are required")
	}
	for _, r := range p.AllowedRoots {
		if !cleanAbs(r) {
			return errors.New("allowed roots must be normalized absolute non-root paths")
		}
	}
	for i, a := range p.Mounts {
		if !cleanAbs(a.Server) || !cleanAbs(a.Local) {
			return errors.New("path mappings require normalized absolute non-root paths")
		}
		for _, b := range p.Mounts[:i] {
			if a.Server == b.Server || a.Local == b.Local {
				return errors.New("ambiguous path mapping")
			}
			// Nested translations must preserve the same relative structure both ways.
			if Within(a.Server, b.Server) {
				rel, _ := filepath.Rel(a.Server, b.Server)
				if filepath.Join(a.Local, rel) != b.Local {
					return errors.New("ambiguous nested path mapping")
				}
			}
			if Within(b.Server, a.Server) {
				rel, _ := filepath.Rel(b.Server, a.Server)
				if filepath.Join(b.Local, rel) != a.Local {
					return errors.New("ambiguous nested path mapping")
				}
			}
			if Within(a.Local, b.Local) && !Within(a.Server, b.Server) || Within(b.Local, a.Local) && !Within(b.Server, a.Server) {
				return errors.New("ambiguous reverse mapping")
			}
		}
	}
	return nil
}
func (p Policy) Local(server string) (string, error) {
	if !cleanAbs(server) {
		return "", errors.New("invalid server path")
	}
	best := ""
	result := ""
	if len(p.Mounts) == 0 {
		result = server
	}
	for _, m := range p.Mounts {
		if Within(m.Server, server) && len(m.Server) > len(best) {
			r, _ := filepath.Rel(m.Server, server)
			best = m.Server
			result = filepath.Join(m.Local, r)
		}
	}
	if result == "" {
		return "", errors.New("server path has no explicit mapping")
	}
	for _, root := range p.AllowedRoots {
		if Within(root, result) {
			return result, nil
		}
	}
	return "", errors.New("path is outside allowed writable roots")
}

// CheckLocal rejects symlinks throughout the entire path, including the allowed
// root. OpenRoot in the writer then keeps later file operations beneath its FD.
func CheckLocal(path string) error {
	if !cleanAbs(path) {
		return errors.New("invalid local path")
	}
	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		cur = filepath.Join(cur, part)
		st, e := os.Lstat(cur)
		if e != nil {
			return fmt.Errorf("unavailable media path: %w", e)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink media path requires a direct mount")
		}
	}
	return nil
}
func video(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".m4v", ".avi", ".mov", ".wmv", ".ts", ".m2ts", ".webm", ".mpg", ".mpeg", ".vob", ".iso", ".strm":
		return true
	}
	return false
}
func (p Policy) Resolve(inv siloapi.Inventory, item siloapi.Item, season *siloapi.Season) ([]Destination, error) {
	if e := p.Validate(); e != nil {
		return nil, e
	}
	if reason := inv.Refusals[item.ID]; reason != "" {
		return nil, errors.New(reason)
	}
	groups := map[string][]siloapi.Membership{}
	seriesSeasons := map[int]bool{}
	for _, f := range inv.Files {
		if f.ItemID != item.ID {
			continue
		}
		if item.Type == "series" {
			seriesSeasons[f.Season] = true
		}
		root := ""
		for _, r := range inv.Library.Paths {
			if Within(r, f.Path) && len(r) > len(root) {
				root = r
			}
		}
		if root == "" {
			return nil, errors.New("file outside configured library roots")
		}
		rel, _ := filepath.Rel(root, f.Path)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 {
			return nil, errors.New("flat library files have no dedicated owner directory")
		}
		owner := filepath.Join(root, parts[0])
		// Keys are library/item/season/copy; -1 denotes a series/movie root.
		sn := -1
		if season != nil {
			sn = season.Number
		}
		key := fmt.Sprintf("%s/%s/%d/%s", inv.Library.ID, item.ID, sn, owner)
		if override := p.Overrides[key]; override != "" {
			if !cleanAbs(override) || override == root || !Within(root, override) || !Within(override, f.Path) {
				return nil, errors.New("invalid destination override")
			}
			owner = override
		}
		groups[owner] = append(groups[owner], f)
	}
	if len(groups) == 0 {
		return nil, errors.New("no local primary media files")
	}
	var result []Destination
	for root, files := range groups {
		local, e := p.Local(root)
		if e != nil {
			return nil, e
		}
		if e = CheckLocal(local); e != nil {
			return nil, e
		}
		known := map[string]siloapi.Membership{}
		for _, f := range inv.Files {
			if Within(root, f.Path) {
				if f.ItemID != item.ID {
					return nil, errors.New("owner directory contains another catalog item")
				}
				l, e := p.Local(f.Path)
				if e != nil {
					return nil, e
				}
				known[l] = f
			}
		}
		seasons := map[int]bool{}
		episodeDirs := map[string]int{}
		var target string
		ownerID := item.ID
		sn := -1
		fallback := false
		for _, f := range files {
			rel, _ := filepath.Rel(root, f.Path)
			parts := strings.Split(rel, string(filepath.Separator))
			parent := filepath.Dir(f.Path)
			if f.ObservedRoot == "" || !(f.ObservedRoot == root || f.ObservedRoot == parent || (len(parts) > 2 && f.ObservedRoot == filepath.Join(root, parts[0]))) {
				return nil, errors.New("missing or conflicting observed root evidence")
			}
			if item.Type == "movie" && len(parts) != 2 && len(parts) != 1 {
				return nil, errors.New("unsupported movie directory layout")
			}
			if item.Type == "movie" && len(parts) == 2 {
				return nil, errors.New("nested movie versions require a supported scanner contract")
			}
			if item.Type == "series" {
				seasons[f.Season] = true
				if len(parts) > 1 {
					match := seasonFolder.FindStringSubmatch(parts[0])
					if len(match) != 2 || len(parts) > 3 {
						return nil, errors.New("unsupported series layout")
					}
					n, _ := strconv.Atoi(match[1])
					if n != f.Season {
						return nil, errors.New("season folder disagrees with catalog membership")
					}
					if len(parts) == 3 {
						dir := filepath.Join(root, parts[0], parts[1])
						if episode, ok := episodeDirs[dir]; ok && episode != f.Episode {
							return nil, errors.New("episode directory contains multiple catalog episodes")
						}
						episodeDirs[dir] = f.Episode
					}
				}
			}
			if season != nil && f.Season == season.Number && len(parts) > 1 {
				dir := filepath.Join(root, parts[0])
				if target != "" && target != dir {
					return nil, errors.New("season has multiple competing directories in one copy")
				}
				target = dir
			}
		}
		if e = filepath.WalkDir(local, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("symlink found beneath media owner")
			}
			if d.IsDir() {
				return nil
			}
			if video(path) {
				if _, ok := known[path]; !ok {
					return errors.New("unknown video beneath media owner")
				}
			}
			return nil
		}); e != nil {
			return nil, e
		}
		for path := range known {
			st, e := os.Stat(path)
			if e != nil || !st.Mode().IsRegular() {
				return nil, errors.New("missing or offline catalog file")
			}
		}
		if season != nil {
			sn = season.Number
			ownerID = season.ID
			if !seasons[sn] {
				continue
			}
			if target == "" {
				if !p.FlatFallback || sn == 0 || len(seriesSeasons) != 1 || !seriesSeasons[sn] || len(seasons) != 1 {
					return nil, errors.New("season has no exclusive directory")
				}
				target = root
				ownerID = item.ID
				fallback = true
			}
			for _, f := range files {
				if Within(target, f.Path) && f.Season != sn {
					return nil, errors.New("season destination contains another season")
				}
			}
		} else {
			target = root
		}
		loc, e := p.Local(target)
		if e != nil {
			return nil, e
		}
		if e = CheckLocal(loc); e != nil {
			return nil, e
		}
		if e = syscall.Access(loc, 2); e != nil {
			return nil, errors.New("theme destination is not writable")
		}
		// File ID, observed roots and catalog membership invalidate old previews.
		sort.Slice(files, func(i, j int) bool { return files[i].ID < files[j].ID })
		b, _ := json.Marshal(files)
		hash := sha256.Sum256(b)
		result = append(result, Destination{inv.Library.ID, item.ID, ownerID, sn, target, loc, root, hex.EncodeToString(hash[:]), fallback})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ServerPath < result[j].ServerPath })
	return result, nil
}
