package main

import (
	"encoding/json"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
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
