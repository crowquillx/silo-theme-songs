package pluginapp

import (
	"context"
	"fmt"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
)

// TestConfiguration checks unsaved settings when an administrator presses
// Check Connection: the API key, the selected libraries and whether Silo can
// write to them. It never changes the running configuration.
func (s *Server) TestConfiguration(ctx context.Context, req *pluginv1.TestConfigurationRequest) (*pluginv1.TestConfigurationResponse, error) {
	fail := func(message string) (*pluginv1.TestConfigurationResponse, error) {
		return &pluginv1.TestConfigurationResponse{Ok: false, Message: message}, nil
	}
	var raw map[string]any
	for _, entry := range req.GetConfig() {
		if entry.GetKey() == "settings" {
			raw = entry.GetValue().AsMap()
		}
	}
	cfg, err := parseSettings(raw)
	if err != nil {
		return fail(err.Error())
	}
	cfg = cfg.withDefaults(ctx, s.manifest.GetPluginId())
	if err := cfg.Validate(); err != nil {
		return fail(err.Error())
	}
	api, err := siloapi.New(cfg.siloURL, cfg.APIKey, cfg.ProfileID)
	if err != nil {
		return fail(err.Error())
	}
	libs, err := api.Libraries(ctx)
	if err != nil {
		return fail(fmt.Sprintf("Theme Songs cannot read Silo's libraries at %s (%v). Check that the API key belongs to an administrator.", cfg.siloURL, err))
	}
	if err := api.ThemeCapability(ctx); err != nil {
		return fail("Silo cannot play theme songs: " + err.Error())
	}
	cfg.Policy = cfg.policy(libs)
	var names, problems []string
	for _, l := range libs {
		if !cfg.selected(l) {
			continue
		}
		names = append(names, l.Name)
		if problem := writable(cfg, l); problem != "" {
			problems = append(problems, problem)
		}
	}
	for _, id := range cfg.LibraryIDs {
		found := false
		for _, l := range libs {
			found = found || l.ID == id
		}
		if !found {
			problems = append(problems, "Library "+id+" does not exist.")
		}
	}
	if len(names) == 0 {
		problems = append(problems, "No movie or series libraries are selected.")
	}
	if len(problems) > 0 {
		if strings.Contains(strings.Join(problems, " "), "read-only") {
			problems = append(problems, "Silo's Docker Compose file mounts media read-only; remove :ro from the media volume and recreate the container.")
		}
		return fail(strings.Join(problems, " "))
	}
	return &pluginv1.TestConfigurationResponse{Ok: true}, nil
}
