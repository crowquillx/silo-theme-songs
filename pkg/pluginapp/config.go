// Package pluginapp is the Theme Songs runtime: scheduled and automatic runs,
// the status page and the settings check.
package pluginapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
	"github.com/hashicorp/go-hclog"
)

type Config struct {
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	ProfileID string `json:"profile_id"`
	StateDir  string `json:"state_dir"`
	// LibraryIDs limits runs to these libraries; empty means every movie and
	// series library. SkipLibraryIDs removes libraries from either set.
	LibraryIDs     []string `json:"library_ids"`
	SkipLibraryIDs []string `json:"skip_library_ids"`
	ExcludeItems   []string `json:"exclude_items"`
	// AnimeLibraryIDs limits AnimeThemes to these libraries; empty means it
	// checks every series.
	AnimeLibraryIDs []string            `json:"anime_library_ids"`
	DryRun          *bool               `json:"dry_run"`
	AnimeThemes     *bool               `json:"anime_themes"`
	FFprobe         string              `json:"ffprobe"`
	MaxDownloads    int                 `json:"max_downloads"`
	Policy          destinations.Policy `json:"destinations"`
	// Provider holds the ThemerrDB and YouTube settings, AnimeProvider the
	// AnimeThemes settings. The switches below override the matching keys.
	Provider      json.RawMessage `json:"provider"`
	AnimeProvider json.RawMessage `json:"anime_provider"`

	ManageTools    *bool `json:"manage_tools"`
	AssistedSearch *bool `json:"assisted_search"`
	AnimeOpenings  *bool `json:"anime_openings"`
	AnimeEndings   *bool `json:"anime_endings"`
	AnimeSpoilers  *bool `json:"anime_spoilers"`
	AnimeNSFW      *bool `json:"anime_nsfw"`

	// Settings from versions before 0.2.0. Silo keeps saved settings that
	// the form no longer shows, so they still arrive. PreviewOnly applies
	// until dry_run is saved; the others are ignored.
	PreviewOnly      *bool  `json:"preview_only"`
	ManualRefresh    *bool  `json:"manual_refresh"`
	SourceGeneration string `json:"source_generation"`

	siloURL  string // BaseURL, or the host's internal API URL when BaseURL is empty
	cacheDir string // Silo's plugin cache directory, when known

	// Log is the plugin's logger, set by the runtime before Factory runs.
	Log hclog.Logger `json:"-"`
}

// Silo's settings form saves each textarea as text, so these settings arrive
// as strings. Lists also accept IDs separated by commas or spaces.
var (
	formTextLists   = []string{"library_ids", "skip_library_ids", "exclude_items", "anime_library_ids"}
	formTextObjects = []string{"destinations", "provider", "anime_provider"}
)

// decodeFormText replaces the text of list and object settings with their
// values. An empty text, or a cleared number, leaves the setting unset.
func decodeFormText(settings map[string]any) error {
	for key, value := range settings {
		if value == nil {
			delete(settings, key)
		}
	}
	for _, key := range append(slices.Clone(formTextLists), formTextObjects...) {
		text, ok := settings[key].(string)
		if !ok {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			delete(settings, key)
			continue
		}
		if slices.Contains(formTextLists, key) && !strings.HasPrefix(text, "[") {
			ids := []any{}
			for _, id := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
				if id = strings.Trim(id, `"'`); id != "" {
					ids = append(ids, id)
				}
			}
			settings[key] = ids
			continue
		}
		var value any
		if e := json.Unmarshal([]byte(text), &value); e != nil {
			return fmt.Errorf("%s is not valid JSON: %v", key, e)
		}
		settings[key] = value
	}
	return nil
}

// fallbackSiloURL is Silo's API listener inside its Docker image.
const fallbackSiloURL = "http://127.0.0.1:8080"

func Defaults() Config {
	return Config{FFprobe: provider.BundledTool("ffprobe")}
}

func on(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

// dryRun reports whether runs only list what they would download.
func (c Config) dryRun() bool {
	if c.DryRun != nil {
		return *c.DryRun
	}
	return on(c.PreviewOnly, false)
}

func (c Config) animeEnabled() bool { return on(c.AnimeThemes, true) }

// selected reports whether runs include a library.
func (c Config) selected(l siloapi.Library) bool {
	if !siloapi.Eligible(l) || slices.Contains(c.SkipLibraryIDs, l.ID) {
		return false
	}
	return len(c.LibraryIDs) == 0 || slices.Contains(c.LibraryIDs, l.ID)
}

// animeLibrary reports whether AnimeThemes may handle series in a library.
func (c Config) animeLibrary(id string) bool {
	return len(c.AnimeLibraryIDs) == 0 || slices.Contains(c.AnimeLibraryIDs, id)
}

// fingerprint identifies the settings a full run used, without the API key.
func (c Config) fingerprint() string {
	c.APIKey = ""
	data, _ := json.Marshal(c)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// withDefaults fills settings Silo can supply. An empty state_dir uses
// plugin-state/<plugin> in Silo's plugin cache directory, which Silo's Docker
// image keeps on its plugin volume.
func (c Config) withDefaults(ctx context.Context, pluginID string) Config {
	c.siloURL = c.BaseURL
	if c.siloURL == "" {
		c.siloURL = fallbackSiloURL
		if host := sdkruntime.Host(); host != nil {
			if info, e := host.GetHostInfo(ctx); e == nil && info.InternalBaseURL != "" {
				c.siloURL = info.InternalBaseURL
			}
		}
	}
	c.cacheDir = pluginCacheDir(pluginID)
	if !filepath.IsAbs(c.cacheDir) {
		c.cacheDir = ""
	}
	if c.StateDir == "" && c.cacheDir != "" && pluginID != "" {
		c.StateDir = filepath.Join(filepath.Clean(c.cacheDir), "plugin-state", pluginID)
	}
	return c
}

// pluginCacheDir returns $SILO_PLUGIN_CACHE_DIR, or "" when it can't be found.
// Silo stopped passing that variable to plugin processes, so when it is unset
// the directory is read from this binary's path.
func pluginCacheDir(pluginID string) string {
	if cache := os.Getenv("SILO_PLUGIN_CACHE_DIR"); cache != "" {
		return cache
	}
	exe, e := os.Executable()
	if e != nil {
		return ""
	}
	return installedCacheDir(exe, pluginID)
}

// installedCacheDir returns the cache directory of a binary Silo installed at
// <cache>/<plugin>/<version>/install-*/plugin, or "" for any other path.
func installedCacheDir(exe, pluginID string) string {
	install := filepath.Dir(filepath.Clean(exe))
	plugin := filepath.Dir(filepath.Dir(install))
	if pluginID == "" || filepath.Base(plugin) != pluginID || !strings.HasPrefix(filepath.Base(install), "install-") {
		return ""
	}
	return filepath.Dir(plugin)
}

// policy returns the destination policy for a run. Without allowed_roots,
// the selected libraries' folders are the writable roots.
func (c Config) policy(libs []siloapi.Library) destinations.Policy {
	p := c.Policy
	if len(p.AllowedRoots) > 0 {
		return p
	}
	for _, l := range libs {
		if !c.selected(l) {
			continue
		}
		for _, server := range l.Paths {
			local := server
			best := ""
			for _, m := range p.Mounts {
				if destinations.Within(m.Server, server) && len(m.Server) > len(best) {
					rel, _ := filepath.Rel(m.Server, server)
					best, local = m.Server, filepath.Join(m.Local, rel)
				}
			}
			if !slices.Contains(p.AllowedRoots, local) {
				p.AllowedRoots = append(p.AllowedRoots, local)
			}
		}
	}
	return p
}

// errNoAPIKey is the first problem of a fresh installation.
var errNoAPIKey = errors.New("Create an API key in Administration → API Keys, paste it into Silo API key in this plugin's settings and save.")

func (c Config) Validate() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return errNoAPIKey
	}
	url := c.siloURL
	if url == "" {
		url = c.BaseURL
	}
	if url == "" {
		url = fallbackSiloURL
	}
	if _, e := siloapi.New(url, c.APIKey, c.ProfileID); e != nil {
		return e
	}
	if c.StateDir == "" {
		return errors.New("Silo did not install the plugin in its plugin directory; set Persistent state directory in Advanced settings")
	}
	if !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir || c.StateDir == "/" {
		return errors.New("Persistent state directory must be a normalized absolute directory")
	}
	if len(c.LibraryIDs) > 100 || len(c.SkipLibraryIDs) > 100 || len(c.AnimeLibraryIDs) > 100 {
		return errors.New("library lists are limited to 100 libraries")
	}
	if c.MaxDownloads < 0 || c.MaxDownloads > 10000 {
		return errors.New("Downloads per run must be between 0 (no limit) and 10000")
	}
	if c.FFprobe == "" {
		return errors.New("ffprobe executable is required")
	}
	return c.Policy.Validate()
}

type Candidate struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	URL        string `json:"-"`
	Extension  string `json:"extension"` // Source format; Fetch returns the normalized output format.
	Provenance string `json:"provenance"`
	Extract    bool   `json:"extract"`
}
type Target struct {
	Item     siloapi.Item
	Season   *siloapi.Season
	Episodes []int
}
type Provider interface {
	Select(context.Context, Target) ([]Candidate, error)
	Fetch(context.Context, Candidate) (*provider.StagedAudio, error)
}

// Claimer is implemented by the AnimeThemes provider. Claims reports whether
// it maps any of a series' seasons, given one target per season.
type Claimer interface {
	Claims(context.Context, []Target) (bool, error)
}

type Factory func(Config) (Provider, error)
