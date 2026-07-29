package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ieshan/codamigo/config"
)

func TestGraphEnabled_DefaultsOn(t *testing.T) {
	if !config.Defaults().GraphEnabled() {
		t.Error("the code graph should be enabled by default")
	}
}

func TestGraphEnabled_UnsetTreatedAsOn(t *testing.T) {
	// A Config built without the field (e.g. by a test or an older caller) must
	// behave as enabled rather than silently disabling the feature.
	if !(&config.Config{}).GraphEnabled() {
		t.Error("an unset EnableGraph should read as enabled")
	}
}

// Merge treats zero values as "not set", so a plain bool could never be turned
// off by an overlay. The pointer must make false stick.
func TestMerge_EnableGraphFalseOverridesDefault(t *testing.T) {
	base := config.Defaults()
	off := false
	merged := base.Merge(&config.Config{EnableGraph: &off})

	if merged.GraphEnabled() {
		t.Error("an explicit false overlay should disable the graph")
	}
	if !base.GraphEnabled() {
		t.Error("Merge must not mutate the receiver")
	}
}

func TestMerge_EnableGraphNilKeepsBase(t *testing.T) {
	off := false
	base := config.Defaults()
	base.EnableGraph = &off

	merged := base.Merge(&config.Config{})
	if merged.GraphEnabled() {
		t.Error("a nil overlay should preserve the base value")
	}
}

func TestMerge_EnableGraphTrueOverridesFalse(t *testing.T) {
	off, on := false, true
	base := config.Defaults()
	base.EnableGraph = &off

	merged := base.Merge(&config.Config{EnableGraph: &on})
	if !merged.GraphEnabled() {
		t.Error("an explicit true overlay should re-enable the graph")
	}
}

func TestLoad_EnableGraphFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yml")
	if err := os.WriteFile(path, []byte("enable_graph: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EnableGraph == nil {
		t.Fatal("enable_graph should be parsed from YAML")
	}
	if cfg.GraphEnabled() {
		t.Error("enable_graph: false should disable the graph")
	}

	// And it must survive the merge onto defaults, which is how main.go uses it.
	if config.Defaults().Merge(cfg).GraphEnabled() {
		t.Error("enable_graph: false should win over the default")
	}
}

func TestLoad_EnableGraphAbsentFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yml")
	if err := os.WriteFile(path, []byte("write_batch_size: 10\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EnableGraph != nil {
		t.Error("an absent enable_graph should stay nil so the default applies")
	}
	if !config.Defaults().Merge(cfg).GraphEnabled() {
		t.Error("the graph should remain enabled when YAML omits the key")
	}
}
