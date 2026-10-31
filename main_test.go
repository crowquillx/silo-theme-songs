package main

import (
	"encoding/json"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/config"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/encoding/protojson"
	"testing"
)

// The host catalog uses standard JSON; runtime uploads use protobuf JSON.
func TestManifestDecodesThroughBothHostPaths(t *testing.T) {
	var catalog, runtime pluginv1.PluginManifest
	if err := json.Unmarshal(manifestJSON, &catalog); err != nil {
		t.Fatalf("catalog manifest: %v", err)
	}
	if err := protojson.Unmarshal(manifestJSON, &runtime); err != nil {
		t.Fatalf("runtime manifest: %v", err)
	}
	if catalog.PluginId == "" || catalog.PluginId != runtime.PluginId {
		t.Fatal("manifest identity mismatch")
	}
}

// Silo validates settings against the manifest schema before the plugin sees
// them, and its settings form saves JSON settings as text.
func TestManifestAcceptsFormText(t *testing.T) {
	var m pluginv1.PluginManifest
	if err := protojson.Unmarshal(manifestJSON, &m); err != nil {
		t.Fatal(err)
	}
	form := map[string]any{"api_key": "key", "library_ids": `["3"]`, "skip_library_ids": "4", "exclude_items": "5", "anime_library_ids": "6", "destinations": `{"allowed_roots":["/media"]}`, "provider": "{}", "anime_provider": `{"op":false}`, "dry_run": false, "anime_themes": true, "anime_openings": true, "anime_endings": false, "anime_spoilers": false, "anime_nsfw": false, "assisted_search": false, "manage_tools": true, "max_downloads": 0}
	if err := config.ValidateManifestGlobalValue(&m, "settings", form); err != nil {
		t.Fatal(err)
	}
	native := map[string]any{"api_key": "key", "library_ids": []any{"3"}, "destinations": map[string]any{"allowed_roots": []any{"/media"}}}
	if err := config.ValidateManifestGlobalValue(&m, "settings", native); err != nil {
		t.Fatal(err)
	}
	// Silo merges saves into the stored settings, so 0.1 settings stay.
	legacy := map[string]any{"api_key": "key", "library_ids": "3", "preview_only": true, "manual_refresh": false, "source_generation": "1", "max_downloads": 10}
	if err := config.ValidateManifestGlobalValue(&m, "settings", legacy); err != nil {
		t.Fatal(err)
	}
}

// Every form field must be in a section, or the form hides it.
func TestFormSectionsCoverEveryField(t *testing.T) {
	var m pluginv1.PluginManifest
	if err := protojson.Unmarshal(manifestJSON, &m); err != nil {
		t.Fatal(err)
	}
	form := m.GetGlobalConfigSchema()[0].GetAdminForm()
	inSection := map[string]int{}
	for _, section := range form.GetSections() {
		for _, key := range section.GetFieldKeys() {
			inSection[key]++
		}
	}
	for _, field := range form.GetFields() {
		if inSection[field.GetKey()] != 1 {
			t.Errorf("field %q is in %d sections", field.GetKey(), inSection[field.GetKey()])
		}
	}
	if !manifest.SupportsConfigurationCheck(&m) {
		t.Error("settings check not declared")
	}
}

// The binary validates its embedded manifest at startup and exits if it fails.
func TestEmbeddedManifestLoads(t *testing.T) {
	if _, err := manifest.LoadWithChecksum(manifestJSON, version); err != nil {
		t.Fatal(err)
	}
}
