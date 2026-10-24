// Package pluginapp is the shared scheduled-task and scan-source runtime.
package pluginapp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
	"github.com/hashicorp/go-hclog"
)

type Config struct {
	BaseURL          string              `json:"base_url"`
	APIKey           string              `json:"api_key"`
	ProfileID        string              `json:"profile_id"`
	StateDir         string              `json:"state_dir"`
	LibraryIDs       []string            `json:"library_ids"`
	ExcludeItems     []string            `json:"exclude_items"`
	AnimeLibraryIDs  []string            `json:"anime_library_ids"`
	PreviewOnly      bool                `json:"preview_only"`
	ManualRefresh    bool                `json:"manual_refresh"`
	SourceGeneration string              `json:"source_generation"`
	FFprobe          string              `json:"ffprobe"`
	MaxDownloads     int                 `json:"max_downloads"`
	Policy           destinations.Policy `json:"destinations"`
	Provider         json.RawMessage     `json:"provider"`

	siloURL string // BaseURL, or the host's internal API URL when BaseURL is empty

	// Log is the plugin's logger, set by the runtime before Factory runs.
	Log hclog.Logger `json:"-"`
}

// fallbackSiloURL is Silo's API listener inside its Docker image.
const fallbackSiloURL = "http://127.0.0.1:8080"

func Defaults() Config {
	return Config{PreviewOnly: true, SourceGeneration: "1", MaxDownloads: 10, FFprobe: provider.BundledTool("ffprobe")}
}

// withDefaults fills settings Silo can supply. An empty state_dir uses
// $SILO_PLUGIN_CACHE_DIR/plugin-state/<plugin>, which Silo's Docker image keeps
// on its plugin volume.
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
	if cache := os.Getenv("SILO_PLUGIN_CACHE_DIR"); c.StateDir == "" && filepath.IsAbs(cache) && pluginID != "" {
		c.StateDir = filepath.Join(filepath.Clean(cache), "plugin-state", pluginID)
	}
	return c
}

// policy returns the destination policy for a run. Without allowed_roots,
// the selected libraries' folders are the writable roots.
func (c Config) policy(libs []siloapi.Library) destinations.Policy {
	p := c.Policy
	if len(p.AllowedRoots) > 0 {
		return p
	}
	for _, l := range libs {
		if !slices.Contains(c.LibraryIDs, l.ID) {
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

func (c Config) Validate() error {
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
		return errors.New("set state_dir to a persistent directory")
	}
	if !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir || c.StateDir == "/" {
		return errors.New("persistent state_dir must be a normalized absolute directory")
	}
	if len(c.LibraryIDs) > 100 {
		return errors.New("at most 100 selected libraries")
	}
	if c.SourceGeneration == "" || len(c.SourceGeneration) > 128 {
		return errors.New("source_generation is required and limited to 128 characters")
	}
	if c.MaxDownloads < 1 || c.MaxDownloads > 100 {
		return errors.New("max_downloads must be between 1 and 100")
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
type Factory func(Config) (Provider, error)
