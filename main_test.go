package main

import (
	"encoding/json"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/config"
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
	form := map[string]any{"api_key": "key", "library_ids": `["3"]`, "exclude_items": "5", "anime_library_ids": "6", "destinations": `{"allowed_roots":["/media"]}`, "provider": "{}"}
	if err := config.ValidateManifestGlobalValue(&m, "settings", form); err != nil {
		t.Fatal(err)
	}
	native := map[string]any{"api_key": "key", "library_ids": []any{"3"}, "destinations": map[string]any{"allowed_roots": []any{"/media"}}}
	if err := config.ValidateManifestGlobalValue(&m, "settings", native); err != nil {
		t.Fatal(err)
	}
}
