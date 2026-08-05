package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieshan/codamigo/config"
)

// ---- Load ----

func TestLoad_ValidYAML(t *testing.T) {
	path := writeTempConfig(t, `
embedding_model: voyage-code-3
embedding_api_key: sk-test
embedding_dimensions: 512
include_patterns:
  - "*.go"
  - "*.ts"
poll_interval: 10s
debounce_window: 250ms
embedding_http_timeout: 30s
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbeddingModel != "voyage-code-3" {
		t.Errorf("EmbeddingModel = %q, want voyage-code-3", cfg.EmbeddingModel)
	}
	if cfg.EmbeddingAPIKey != "sk-test" {
		t.Errorf("EmbeddingAPIKey = %q, want sk-test", cfg.EmbeddingAPIKey)
	}
	if cfg.EmbeddingDimensions != 512 {
		t.Errorf("EmbeddingDimensions = %d, want 512", cfg.EmbeddingDimensions)
	}
	if len(cfg.IncludePatterns) != 2 {
		t.Errorf("IncludePatterns length = %d, want 2", len(cfg.IncludePatterns))
	}
	if cfg.PollInterval != 10*time.Second {
		t.Errorf("PollInterval = %v, want 10s", cfg.PollInterval)
	}
	if cfg.DebounceWindow != 250*time.Millisecond {
		t.Errorf("DebounceWindow = %v, want 250ms", cfg.DebounceWindow)
	}
	if cfg.EmbeddingHTTPTimeout != 30*time.Second {
		t.Errorf("EmbeddingHTTPTimeout = %v, want 30s", cfg.EmbeddingHTTPTimeout)
	}
}

func TestLoad_UnknownKey(t *testing.T) {
	path := writeTempConfig(t, "unknown_key: value\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
}

func TestLoad_BadDuration(t *testing.T) {
	path := writeTempConfig(t, "poll_interval: notaduration\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for bad duration, got nil")
	}
}

func TestLoad_BadEmbeddingHTTPTimeout(t *testing.T) {
	path := writeTempConfig(t, "embedding_http_timeout: notaduration\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for bad embedding_http_timeout, got nil")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nonexistent.yml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	path := writeTempConfig(t, "")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load empty file: %v", err)
	}
	if cfg.EmbeddingModel != "" {
		t.Errorf("EmbeddingModel = %q, want empty for zero config", cfg.EmbeddingModel)
	}
}

// ---- LoadOrDefault ----

func TestLoadOrDefault_MissingFile(t *testing.T) {
	cfg, err := config.LoadOrDefault(filepath.Join(t.TempDir(), "nonexistent.yml"))
	if err != nil {
		t.Fatalf("LoadOrDefault missing file: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.EmbeddingModel != "" {
		t.Errorf("EmbeddingModel = %q, want empty", cfg.EmbeddingModel)
	}
}

func TestLoadOrDefault_ValidFile(t *testing.T) {
	path := writeTempConfig(t, "embedding_model: custom-model\n")
	cfg, err := config.LoadOrDefault(path)
	if err != nil {
		t.Fatalf("LoadOrDefault valid file: %v", err)
	}
	if cfg.EmbeddingModel != "custom-model" {
		t.Errorf("EmbeddingModel = %q, want custom-model", cfg.EmbeddingModel)
	}
}

// ---- Merge ----

func TestMerge_ScalarOverride(t *testing.T) {
	base := &config.Config{EmbeddingModel: "model-a", EmbeddingDimensions: 1536}
	overlay := &config.Config{EmbeddingModel: "model-b"}
	result := base.Merge(overlay)
	if result.EmbeddingModel != "model-b" {
		t.Errorf("EmbeddingModel = %q, want model-b", result.EmbeddingModel)
	}
	if result.EmbeddingDimensions != 1536 {
		t.Errorf("EmbeddingDimensions = %d, want 1536 (unchanged)", result.EmbeddingDimensions)
	}
}

func TestMerge_ZeroOverlayDoesNotOverride(t *testing.T) {
	base := &config.Config{EmbeddingModel: "model-a", EmbeddingRateLimit: 200}
	overlay := &config.Config{} // all zero
	result := base.Merge(overlay)
	if result.EmbeddingModel != "model-a" {
		t.Errorf("EmbeddingModel = %q, want model-a", result.EmbeddingModel)
	}
	if result.EmbeddingRateLimit != 200 {
		t.Errorf("EmbeddingRateLimit = %v, want 200", result.EmbeddingRateLimit)
	}
}

func TestMerge_DurationOverride(t *testing.T) {
	base := &config.Config{PollInterval: 5 * time.Second}
	overlay := &config.Config{PollInterval: 30 * time.Second}
	result := base.Merge(overlay)
	if result.PollInterval != 30*time.Second {
		t.Errorf("PollInterval = %v, want 30s", result.PollInterval)
	}
}

func TestMerge_EmbeddingHTTPTimeoutOverlay(t *testing.T) {
	base := config.Defaults()
	overlay := &config.Config{EmbeddingHTTPTimeout: 120 * time.Second}
	result := base.Merge(overlay)
	if result.EmbeddingHTTPTimeout != 120*time.Second {
		t.Errorf("EmbeddingHTTPTimeout = %v, want 120s", result.EmbeddingHTTPTimeout)
	}
}

func TestMerge_SliceNilDoesNotOverride(t *testing.T) {
	base := &config.Config{IncludePatterns: []string{"*.go"}}
	overlay := &config.Config{} // nil slice, not set
	result := base.Merge(overlay)
	if len(result.IncludePatterns) != 1 || result.IncludePatterns[0] != "*.go" {
		t.Errorf("IncludePatterns = %v, want [*.go]", result.IncludePatterns)
	}
}

func TestMerge_EmptySliceOverrides(t *testing.T) {
	base := &config.Config{IncludePatterns: []string{"*.go"}}
	overlay := &config.Config{IncludePatterns: []string{}} // explicit empty
	result := base.Merge(overlay)
	if len(result.IncludePatterns) != 0 {
		t.Errorf("IncludePatterns = %v, want [] (explicitly cleared)", result.IncludePatterns)
	}
}

func TestMerge_DoesNotMutateBase(t *testing.T) {
	base := &config.Config{EmbeddingModel: "model-a"}
	overlay := &config.Config{EmbeddingModel: "model-b"}
	_ = base.Merge(overlay)
	if base.EmbeddingModel != "model-a" {
		t.Errorf("base mutated: EmbeddingModel = %q, want model-a", base.EmbeddingModel)
	}
}

// ---- Defaults ----

func TestDefaults(t *testing.T) {
	d := config.Defaults()
	if d.EmbeddingModel != "text-embedding-3-small" {
		t.Errorf("EmbeddingModel = %q", d.EmbeddingModel)
	}
	if d.EmbeddingBaseURL != "https://api.openai.com/v1" {
		t.Errorf("EmbeddingBaseURL = %q", d.EmbeddingBaseURL)
	}
	if d.EmbeddingDimensions != 1536 {
		t.Errorf("EmbeddingDimensions = %d", d.EmbeddingDimensions)
	}
	if d.EmbeddingMaxBatchSize != 256 {
		t.Errorf("EmbeddingMaxBatchSize = %d", d.EmbeddingMaxBatchSize)
	}
	if d.EmbeddingRateLimit != 500 {
		t.Errorf("EmbeddingRateLimit = %v", d.EmbeddingRateLimit)
	}
	if d.EmbeddingRateBurst != 100 {
		t.Errorf("EmbeddingRateBurst = %d", d.EmbeddingRateBurst)
	}
	if d.EmbeddingMaxRetries != 3 {
		t.Errorf("EmbeddingMaxRetries = %d", d.EmbeddingMaxRetries)
	}
	if d.EmbeddingRetryBaseDelay != 500*time.Millisecond {
		t.Errorf("EmbeddingRetryBaseDelay = %v", d.EmbeddingRetryBaseDelay)
	}
	if d.WatchMode != "auto" {
		t.Errorf("WatchMode = %q", d.WatchMode)
	}
	if d.PollInterval != 5*time.Second {
		t.Errorf("PollInterval = %v", d.PollInterval)
	}
	if d.DebounceWindow != 500*time.Millisecond {
		t.Errorf("DebounceWindow = %v", d.DebounceWindow)
	}
	if d.IndexConcurrency != 20 {
		t.Errorf("IndexConcurrency = %d", d.IndexConcurrency)
	}
	if d.ProjectRoot != "" {
		t.Errorf("ProjectRoot = %q, want empty (resolved at runtime)", d.ProjectRoot)
	}
	if d.EmbeddingHTTPTimeout != 60*time.Second {
		t.Errorf("EmbeddingHTTPTimeout = %v, want 60s", d.EmbeddingHTTPTimeout)
	}
	if d.StaleRefreshThreshold != 10 {
		t.Errorf("StaleRefreshThreshold = %d, want 10", d.StaleRefreshThreshold)
	}
}

func TestMerge_StaleRefreshThreshold(t *testing.T) {
	base := config.Defaults()

	// Non-zero overlay overrides the default.
	got := base.Merge(&config.Config{StaleRefreshThreshold: 25})
	if got.StaleRefreshThreshold != 25 {
		t.Errorf("StaleRefreshThreshold = %d, want 25", got.StaleRefreshThreshold)
	}

	// Zero overlay leaves the base value intact.
	got = base.Merge(&config.Config{})
	if got.StaleRefreshThreshold != 10 {
		t.Errorf("StaleRefreshThreshold = %d, want 10 (default preserved)", got.StaleRefreshThreshold)
	}
}

func TestValidate_NegativeStaleRefreshThreshold(t *testing.T) {
	c := &config.Config{StaleRefreshThreshold: -1}
	if err := c.Validate(); err == nil {
		t.Error("expected error for negative StaleRefreshThreshold")
	}
}

func TestMerge_SliceNotAliased(t *testing.T) {
	overlay := &config.Config{IncludePatterns: []string{"*.go", "*.ts"}}
	base := &config.Config{}
	result := base.Merge(overlay)

	// Mutating the overlay's slice must not affect the merged result.
	overlay.IncludePatterns[0] = "MUTATED"
	if result.IncludePatterns[0] == "MUTATED" {
		t.Error("Merge aliased IncludePatterns slice; mutation leaked to result")
	}
}

// ---- helpers ----

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yml")
	if err != nil {
		t.Fatalf("creating temp config: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing temp config: %v", err)
	}
	return f.Name()
}

// ---- godoc examples ----

func ExampleDefaults() {
	cfg := config.Defaults()
	fmt.Println(cfg.EmbeddingModel)
	fmt.Println(cfg.WatchMode)
	fmt.Println(cfg.PollInterval)
	// Output:
	// text-embedding-3-small
	// auto
	// 5s
}

func ExampleConfig_Merge() {
	base := config.Defaults()
	overlay := &config.Config{
		EmbeddingModel: "voyage-code-3",
		WatchMode:      "poll",
	}
	merged := base.Merge(overlay)
	fmt.Println(merged.EmbeddingModel)
	fmt.Println(merged.WatchMode)
	fmt.Println(merged.EmbeddingDimensions) // unchanged from base
	// Output:
	// voyage-code-3
	// poll
	// 1536
}

func ExampleLoad() {
	f, err := os.CreateTemp("", "example-*.yml")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString("embedding_model: my-model\npoll_interval: 10s\n"); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}

	cfg, err := config.Load(f.Name())
	if err != nil {
		panic(err)
	}
	fmt.Println(cfg.EmbeddingModel)
	fmt.Println(cfg.PollInterval)
	// Output:
	// my-model
	// 10s
}

func ExampleLoadOrDefault() {
	cfg, err := config.LoadOrDefault("/nonexistent/path.yml")
	if err != nil {
		panic(err)
	}
	fmt.Println(cfg.EmbeddingModel) // zero value, not defaults
	// Output:
	//
}

// ---- Validate ----

func TestValidate_ValidConfig(t *testing.T) {
	cfg := config.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on defaults: %v", err)
	}
}

func TestValidate_InvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*config.Config)
		errStr string
	}{
		{"negative dimensions", func(c *config.Config) { c.EmbeddingDimensions = -1 }, "EmbeddingDimensions"},
		{"bad watch mode", func(c *config.Config) { c.WatchMode = "invalid" }, "WatchMode"},
		{"model without provider", func(c *config.Config) { c.EmbeddingProvider = ""; c.EmbeddingModel = "m" }, "EmbeddingProvider"},
		{"negative concurrency", func(c *config.Config) { c.IndexConcurrency = -1 }, "IndexConcurrency"},
		{"negative max file size", func(c *config.Config) { c.MaxFileSize = -1 }, "MaxFileSize"},
		{"negative poll interval", func(c *config.Config) { c.PollInterval = -1 }, "PollInterval"},
		{"negative debounce window", func(c *config.Config) { c.DebounceWindow = -1 }, "DebounceWindow"},
		{"negative retry base delay", func(c *config.Config) { c.EmbeddingRetryBaseDelay = -1 }, "EmbeddingRetryBaseDelay"},
		{"negative http timeout", func(c *config.Config) { c.EmbeddingHTTPTimeout = -1 }, "EmbeddingHTTPTimeout"},
		{"malformed embedding URL", func(c *config.Config) { c.EmbeddingBaseURL = "not a url" }, "EmbeddingBaseURL"},
		{"ftp embedding URL", func(c *config.Config) { c.EmbeddingBaseURL = "ftp://example.com" }, "EmbeddingBaseURL"},
		{"empty host embedding URL", func(c *config.Config) { c.EmbeddingBaseURL = "http://" }, "EmbeddingBaseURL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			tt.modify(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errStr) {
				t.Errorf("error %q does not mention %q", err, tt.errStr)
			}
		})
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := &config.Config{
		EmbeddingDimensions: -1,
		IndexConcurrency:    -1,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "EmbeddingDimensions") || !strings.Contains(err.Error(), "IndexConcurrency") {
		t.Errorf("expected both violations in error: %v", err)
	}
}

func TestValidate_ZeroDimensionsWithModel(t *testing.T) {
	cfg := &config.Config{
		EmbeddingProvider:   "openai",
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 0,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for zero dimensions with model set, got nil")
	}
	if !strings.Contains(err.Error(), "EmbeddingDimensions must be > 0") {
		t.Errorf("error %q does not contain expected message", err)
	}
}

func TestValidate_ValidHTTPSEmbeddingURL(t *testing.T) {
	cfg := config.Defaults()
	cfg.EmbeddingBaseURL = "https://api.openai.com"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error for valid HTTPS URL, got: %v", err)
	}
}

// TestValidate_ValidHTTPEmbeddingURL verifies that plain http:// URLs are accepted,
// e.g. for local or self-hosted embedders that do not use TLS.
func TestValidate_ValidHTTPEmbeddingURL(t *testing.T) {
	cfg := config.Defaults()
	cfg.EmbeddingBaseURL = "http://localhost:11434/v1"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error for valid HTTP URL, got: %v", err)
	}
}

func TestValidate_ZeroDimensionsWithoutModel(t *testing.T) {
	cfg := &config.Config{
		EmbeddingDimensions: 0,
	}
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no error for zero dimensions without model, got: %v", err)
	}
}

func TestGlobalConfigPath_RelativeXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	_, err := config.GlobalConfigPath()
	if err == nil {
		t.Fatal("expected error for relative XDG_CONFIG_HOME, got nil")
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("error %q does not mention 'absolute path'", err)
	}
}

func TestGlobalConfigPath_AbsoluteXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/absolute/xdg")
	p, err := config.GlobalConfigPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/absolute/xdg", "codamigo", "global_settings.yml")
	if p != want {
		t.Errorf("path = %q, want %q", p, want)
	}
}

func TestGlobalConfigPath_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // Windows fallback
	t.Setenv("XDG_CONFIG_HOME", "")
	_, err := config.GlobalConfigPath()
	if err == nil {
		t.Fatal("expected error when HOME is unset")
	}
}

func TestGlobalConfigPath_XDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
	p, err := config.GlobalConfigPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "/custom/xdg/codamigo/global_settings.yml" {
		t.Errorf("path = %q, want /custom/xdg/codamigo/global_settings.yml", p)
	}
}

func TestMerge_NonCodeLanguagesNilPreservesBase(t *testing.T) {
	base := &config.Config{NonCodeLanguages: []string{"markdown", "yaml"}}
	overlay := &config.Config{} // nil slice
	result := base.Merge(overlay)
	if len(result.NonCodeLanguages) != 2 || result.NonCodeLanguages[0] != "markdown" {
		t.Errorf("NonCodeLanguages = %v, want [markdown yaml]", result.NonCodeLanguages)
	}
}

func TestMerge_NonCodeLanguagesOverride(t *testing.T) {
	base := &config.Config{NonCodeLanguages: []string{"markdown", "yaml"}}
	overlay := &config.Config{NonCodeLanguages: []string{"json"}}
	result := base.Merge(overlay)
	if len(result.NonCodeLanguages) != 1 || result.NonCodeLanguages[0] != "json" {
		t.Errorf("NonCodeLanguages = %v, want [json]", result.NonCodeLanguages)
	}
}

func TestMerge_NonCodeLanguagesEmptyClears(t *testing.T) {
	base := &config.Config{NonCodeLanguages: []string{"markdown", "yaml"}}
	overlay := &config.Config{NonCodeLanguages: []string{}}
	result := base.Merge(overlay)
	if len(result.NonCodeLanguages) != 0 {
		t.Errorf("NonCodeLanguages = %v, want [] (explicitly cleared)", result.NonCodeLanguages)
	}
}

func TestDefaults_NonCodeLanguages(t *testing.T) {
	d := config.Defaults()
	want := []string{"markdown", "yaml", "json"}
	if len(d.NonCodeLanguages) != len(want) {
		t.Fatalf("NonCodeLanguages length = %d, want %d", len(d.NonCodeLanguages), len(want))
	}
	for i, lang := range want {
		if d.NonCodeLanguages[i] != lang {
			t.Errorf("NonCodeLanguages[%d] = %q, want %q", i, d.NonCodeLanguages[i], lang)
		}
	}
}

func TestMerge_NonCodeLanguagesNotAliased(t *testing.T) {
	overlay := &config.Config{NonCodeLanguages: []string{"markdown", "yaml"}}
	base := &config.Config{}
	result := base.Merge(overlay)
	overlay.NonCodeLanguages[0] = "MUTATED"
	if result.NonCodeLanguages[0] == "MUTATED" {
		t.Error("Merge aliased NonCodeLanguages slice; mutation leaked to result")
	}
}

func TestWriteBatchSize_Defaults(t *testing.T) {
	cfg := config.Defaults()
	if cfg.WriteBatchSize != 50 {
		t.Errorf("WriteBatchSize default = %d; want 50", cfg.WriteBatchSize)
	}
}

func TestWriteBatchSize_Merge(t *testing.T) {
	base := config.Defaults()
	overlay := &config.Config{WriteBatchSize: 100}
	merged := base.Merge(overlay)
	if merged.WriteBatchSize != 100 {
		t.Errorf("WriteBatchSize after merge = %d; want 100", merged.WriteBatchSize)
	}
}

func TestWriteBatchSize_Validate(t *testing.T) {
	cfg := config.Defaults()
	cfg.WriteBatchSize = -1
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for negative WriteBatchSize")
	}
}

// ---- Project hash and paths ----

func TestProjectHash(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"trailing slash", "/home/user1/project1/"},
		{"no trailing slash", "/home/user1/project1"},
		{"leading slash only", "/home"},
		{"root", "/"},
		{"empty", ""},
	}
	hashes := make(map[string]string, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := config.ProjectHash(tt.path)
			hashes[tt.path] = h
			if len(h) != 40 {
				t.Errorf("hash length = %d, want 40", len(h))
			}
			for _, c := range h {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Errorf("hash contains non-hex char %q in %q", c, h)
				}
			}
		})
	}

	// Trailing slash and no trailing slash must produce the same hash.
	if hashes["/home/user1/project1/"] != hashes["/home/user1/project1"] {
		t.Errorf("trailing slash should not affect hash: %q vs %q",
			hashes["/home/user1/project1/"], hashes["/home/user1/project1"])
	}

	// Different paths must produce different hashes.
	if hashes["/home/user1/project1"] == hashes["/home"] {
		t.Error("different paths produced the same hash")
	}

	// Deterministic: re-compute and compare.
	h1 := config.ProjectHash("/home/user1/project1")
	h2 := config.ProjectHash("/home/user1/project1")
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h1, h2)
	}
}

func TestDefaultStorePath(t *testing.T) {
	p, err := config.DefaultStorePath("/home/user/project")
	if err != nil {
		t.Fatalf("DefaultStorePath: %v", err)
	}
	if !strings.HasSuffix(p, "/store.db") {
		t.Errorf("path %q should end with /store.db", p)
	}
	if !strings.Contains(p, ".codamigo/projects/") {
		t.Errorf("path %q should contain .codamigo/projects/", p)
	}
	// Verify the hash segment is 40 hex chars.
	parts := strings.Split(p, string(filepath.Separator))
	var hashSegment string
	for _, part := range parts {
		if len(part) == 40 {
			hashSegment = part
			break
		}
	}
	if hashSegment == "" {
		t.Errorf("path %q should contain a 40-char hash segment", p)
	}
	// Determinism.
	p2, _ := config.DefaultStorePath("/home/user/project")
	if p != p2 {
		t.Errorf("DefaultStorePath not deterministic: %q vs %q", p, p2)
	}
}

func TestProjectDataDir_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	_, err := config.ProjectDataDir("/some/path")
	if err == nil {
		t.Fatal("expected error when home directory cannot be determined")
	}
	if !strings.Contains(err.Error(), "home directory") {
		t.Errorf("error should mention home directory, got: %v", err)
	}
}

func TestHomeProjectConfigPath(t *testing.T) {
	p, err := config.HomeProjectConfigPath("/home/user/project")
	if err != nil {
		t.Fatalf("HomeProjectConfigPath: %v", err)
	}
	if !strings.HasSuffix(p, "/settings.yml") {
		t.Errorf("path %q should end with /settings.yml", p)
	}
	if !strings.Contains(p, ".codamigo/projects/") {
		t.Errorf("path %q should contain .codamigo/projects/", p)
	}
}
