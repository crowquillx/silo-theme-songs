// Package pluginapp is the shared scheduled-task and scan-source runtime.
package pluginapp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
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
}

func Defaults() Config {
	return Config{PreviewOnly: true, SourceGeneration: "1", MaxDownloads: 10, FFprobe: "ffprobe"}
}
func (c Config) Validate() error {
	if _, e := siloapi.New(c.BaseURL, c.APIKey, c.ProfileID); e != nil {
		return e
	}
	if !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir || c.StateDir == "/" {
		return errors.New("persistent state_dir must be a normalized absolute directory")
	}
	if len(c.LibraryIDs) == 0 {
		return errors.New("choose at least one library_id")
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
