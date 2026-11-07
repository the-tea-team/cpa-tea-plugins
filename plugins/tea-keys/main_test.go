package main

import "testing"

func TestBuildPluginMetadata(t *testing.T) {
	plugin, err := buildPlugin([]byte("keys:\n  - key: sk-test\n"), t.TempDir())
	if err != nil {
		t.Fatalf("buildPlugin returned error: %v", err)
	}
	if plugin.Metadata.Version != pluginVersion {
		t.Fatalf("metadata version %q != pluginVersion %q", plugin.Metadata.Version, pluginVersion)
	}
	if plugin.Metadata.Name != pluginName {
		t.Fatalf("metadata name %q != pluginName %q", plugin.Metadata.Name, pluginName)
	}
}
