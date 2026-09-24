package destinations

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
)

func fixture(t *testing.T) (Policy, siloapi.Inventory, siloapi.Item) {
	t.Helper()
	root := t.TempDir()
	show := filepath.Join(root, "Show")
	var files []siloapi.Membership
	for i, name := range []string{"Season 01/a.mkv", "Season 02/b.mkv"} {
		path := filepath.Join(show, name)
		if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, []byte("video"), 0644); e != nil {
			t.Fatal(e)
		}
		files = append(files, siloapi.Membership{File: siloapi.File{ID: name, LibraryID: "1", Path: path, ObservedRoot: show}, ItemID: "show", Kind: "series", Season: i + 1, SeasonID: name})
	}
	return Policy{AllowedRoots: []string{root}}, siloapi.Inventory{Library: siloapi.Library{ID: "1", Type: "series", Paths: []string{root}}, Files: files, Refusals: map[string]string{}}, siloapi.Item{ID: "show", Type: "series"}
}
func TestSeparateSeasonsAndUnknownVideo(t *testing.T) {
	p, inv, item := fixture(t)
	s := siloapi.Season{ID: "s2", Number: 2}
	d, e := p.Resolve(inv, item, &s)
	if e != nil || len(d) != 1 || d[0].OwnerID != "s2" || filepath.Base(d[0].LocalPath) != "Season 02" {
		t.Fatalf("%+v %v", d, e)
	}
	os.WriteFile(filepath.Join(filepath.Dir(d[0].LocalPath), "unknown.mkv"), nil, 0644)
	if _, e = p.Resolve(inv, item, &s); e == nil {
		t.Fatal("accepted unknown video")
	}
}
func TestForeignItemAndWrongSeason(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		p, inv, item := fixture(t)
		if foreign {
			inv.Files[1].ItemID = "other"
		} else {
			inv.Files[1].Season = 8
		}
		if _, e := p.Resolve(inv, item, nil); e == nil {
			t.Fatal("accepted conflicting ownership")
		}
	}
}
func TestMountBoundariesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	p := Policy{AllowedRoots: []string{root}, Mounts: []Mount{{"/media", root}}}
	if _, e := p.Local("/media-other/show"); e == nil {
		t.Fatal("prefix escaped")
	}
	if e := os.Symlink(t.TempDir(), filepath.Join(root, "show")); e != nil {
		t.Fatal(e)
	}
	if e := CheckLocal(filepath.Join(root, "show")); e == nil {
		t.Fatal("accepted symlink")
	}
	p.Mounts = append(p.Mounts, Mount{"/media/anime", filepath.Join(root, "elsewhere")})
	if e := p.Validate(); e == nil {
		t.Fatal("accepted inconsistent reverse mapping")
	}
}
func TestExplicitMappingsRequireCoverageIncludingLocalPaths(t *testing.T) {
	root := t.TempDir()
	p := Policy{AllowedRoots: []string{root}, Mounts: []Mount{{Server: "/server", Local: filepath.Join(root, "mapped")}}}
	if _, e := p.Local(filepath.Join(root, "mapped", "Show")); e == nil {
		t.Fatal("accepted implicit identity alias of a mapped destination")
	}
	if got, e := p.Local("/server/Show"); e != nil || got != filepath.Join(root, "mapped", "Show") {
		t.Fatalf("explicit translation = %q, %v", got, e)
	}
	p.Mounts = append(p.Mounts, Mount{Server: filepath.Join(root, "direct"), Local: filepath.Join(root, "direct")})
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	if got, e := p.Local(filepath.Join(root, "direct", "Show")); e != nil || got != filepath.Join(root, "direct", "Show") {
		t.Fatalf("explicit identity translation = %q, %v", got, e)
	}
	if _, e := p.Local(filepath.Join(root, "mapped", "Show")); e == nil {
		t.Fatal("identity mapping outside its declared prefix was accepted")
	}
}
func TestFlatFallbackRequiresOneRegularSeason(t *testing.T) {
	p, inv, item := fixture(t)
	show := filepath.Dir(filepath.Dir(inv.Files[0].Path))
	for i := range inv.Files {
		old := inv.Files[i].Path
		path := filepath.Join(show, filepath.Base(old))
		if e := os.Rename(old, path); e != nil {
			t.Fatal(e)
		}
		inv.Files[i].Path = path
	}
	s := siloapi.Season{ID: "s1", Number: 1}
	p.FlatFallback = true
	if _, e := p.Resolve(inv, item, &s); e == nil {
		t.Fatal("mixed-season fallback accepted")
	}
	inv.Files[1].Season = 1
	d, e := p.Resolve(inv, item, &s)
	if e != nil || len(d) != 1 || !d[0].Fallback || d[0].OwnerID != item.ID {
		t.Fatalf("%+v %v", d, e)
	}
	inv.Files[0].Season = 0
	inv.Files[1].Season = 0
	s.Number = 0
	if _, e := p.Resolve(inv, item, &s); e == nil {
		t.Fatal("specials fallback accepted")
	}
}
func TestFlatFallbackChecksAllSeriesCopies(t *testing.T) {
	root := t.TempDir()
	inv := siloapi.Inventory{Library: siloapi.Library{ID: "lib", Type: "series", Paths: []string{root}}, Refusals: map[string]string{}}
	item := siloapi.Item{ID: "show", Type: "series"}
	for i, copy := range []string{"Copy A", "Copy B"} {
		owner := filepath.Join(root, copy)
		if e := os.MkdirAll(owner, 0755); e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(owner, "episode.mkv")
		if e := os.WriteFile(path, []byte("video"), 0644); e != nil {
			t.Fatal(e)
		}
		inv.Files = append(inv.Files, siloapi.Membership{File: siloapi.File{ID: copy, LibraryID: "lib", Path: path, ObservedRoot: owner}, ItemID: item.ID, Kind: "series", Season: i + 1, Episode: 1})
	}
	p := Policy{AllowedRoots: []string{root}, FlatFallback: true}
	s := siloapi.Season{ID: "season-1", Number: 1}
	if _, e := p.Resolve(inv, item, &s); e == nil {
		t.Fatal("accepted flat season 1 although copy B has season 2")
	}
	inv.Files[1].Season = 1
	if got, e := p.Resolve(inv, item, &s); e != nil || len(got) != 2 || !got[0].Fallback || !got[1].Fallback {
		t.Fatalf("same-season copies should each use flat fallback: %+v %v", got, e)
	}
	inv.Files[1].Season = 0
	if _, e := p.Resolve(inv, item, &s); e == nil {
		t.Fatal("accepted flat season with specials in another copy")
	}
}

func TestNestedEpisodeDirectoryHasOneEpisodeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dirs      []string
		episodes  []int
		wantError bool
	}{
		{"separate episode directories", []string{"Episode 1", "Episode 2"}, []int{1, 2}, false},
		{"multiple files for one episode", []string{"Episode 1", "Episode 1"}, []int{1, 1}, false},
		{"release pack mixes episodes", []string{"Release Pack", "Release Pack"}, []int{1, 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			owner := filepath.Join(root, "Show")
			seasonPath := filepath.Join(owner, "Season 01")
			inv := siloapi.Inventory{Library: siloapi.Library{ID: "lib", Type: "series", Paths: []string{root}}, Refusals: map[string]string{}}
			for i, dir := range tc.dirs {
				path := filepath.Join(seasonPath, dir, string(rune('a'+i))+".mkv")
				if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(path, []byte("video"), 0644); e != nil {
					t.Fatal(e)
				}
				inv.Files = append(inv.Files, siloapi.Membership{File: siloapi.File{ID: path, LibraryID: "lib", Path: path, ObservedRoot: seasonPath}, ItemID: "show", Kind: "series", Season: 1, Episode: tc.episodes[i]})
			}
			p := Policy{AllowedRoots: []string{root}}
			dest, e := p.Resolve(inv, siloapi.Item{ID: "show", Type: "series"}, &siloapi.Season{ID: "season-1", Number: 1})
			if (e != nil) != tc.wantError {
				t.Fatalf("destinations=%+v error=%v, want error=%v", dest, e, tc.wantError)
			}
			if !tc.wantError && (len(dest) != 1 || dest[0].LocalPath != seasonPath) {
				t.Fatalf("wrong season destination: %+v", dest)
			}
		})
	}
}
