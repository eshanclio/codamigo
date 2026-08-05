package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieshan/codamigo/config"
)

func TestLoad_LocalEmbeddingFields(t *testing.T) {
	path := writeTempConfig(t, `
embedding_provider: local
embedding_model: bge-small-en-v1.5
embedding_hf_token: hf_secret
embedding_local_backend: xla
embedding_local_model_dir: /models
embedding_local_max_seq_len: 256
embedding_local_batch_size: 16
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbeddingProvider != "local" {
		t.Errorf("EmbeddingProvider = %q, want local", cfg.EmbeddingProvider)
	}
	if cfg.EmbeddingHFToken != "hf_secret" {
		t.Errorf("EmbeddingHFToken = %q, want hf_secret", cfg.EmbeddingHFToken)
	}
	if cfg.EmbeddingLocalBackend != "xla" {
		t.Errorf("EmbeddingLocalBackend = %q, want xla", cfg.EmbeddingLocalBackend)
	}
	if cfg.EmbeddingLocalModelDir != "/models" {
		t.Errorf("EmbeddingLocalModelDir = %q, want /models", cfg.EmbeddingLocalModelDir)
	}
	if cfg.EmbeddingLocalMaxSeqLen != 256 {
		t.Errorf("EmbeddingLocalMaxSeqLen = %d, want 256", cfg.EmbeddingLocalMaxSeqLen)
	}
	if cfg.EmbeddingLocalBatchSize != 16 {
		t.Errorf("EmbeddingLocalBatchSize = %d, want 16", cfg.EmbeddingLocalBatchSize)
	}
}

func TestDefaults_LocalEmbeddingFields(t *testing.T) {
	cfg := config.Defaults()
	if cfg.EmbeddingLocalBackend != "auto" {
		t.Errorf("EmbeddingLocalBackend = %q, want auto", cfg.EmbeddingLocalBackend)
	}
	if cfg.EmbeddingLocalMaxSeqLen != config.MaxLocalSeqLen {
		t.Errorf("EmbeddingLocalMaxSeqLen = %d, want %d", cfg.EmbeddingLocalMaxSeqLen, config.MaxLocalSeqLen)
	}
	if cfg.EmbeddingLocalBatchSize != 32 {
		t.Errorf("EmbeddingLocalBatchSize = %d, want 32", cfg.EmbeddingLocalBatchSize)
	}
	// Left empty on purpose: resolving it needs the home directory, which can
	// fail, and Defaults returns no error.
	if cfg.EmbeddingLocalModelDir != "" {
		t.Errorf("EmbeddingLocalModelDir = %q, want empty", cfg.EmbeddingLocalModelDir)
	}
	if cfg.EmbeddingHFToken != "" {
		t.Errorf("EmbeddingHFToken = %q, want empty", cfg.EmbeddingHFToken)
	}
}

func TestMerge_LocalEmbeddingFields(t *testing.T) {
	out := config.Defaults().Merge(&config.Config{
		EmbeddingLocalBackend:   "go",
		EmbeddingLocalMaxSeqLen: 128,
		EmbeddingLocalBatchSize: 8,
		EmbeddingLocalModelDir:  "/elsewhere",
		EmbeddingHFToken:        "hf_x",
	})
	if out.EmbeddingLocalBackend != "go" {
		t.Errorf("EmbeddingLocalBackend = %q, want go", out.EmbeddingLocalBackend)
	}
	if out.EmbeddingLocalMaxSeqLen != 128 {
		t.Errorf("EmbeddingLocalMaxSeqLen = %d, want 128", out.EmbeddingLocalMaxSeqLen)
	}
	if out.EmbeddingLocalBatchSize != 8 {
		t.Errorf("EmbeddingLocalBatchSize = %d, want 8", out.EmbeddingLocalBatchSize)
	}
	if out.EmbeddingLocalModelDir != "/elsewhere" {
		t.Errorf("EmbeddingLocalModelDir = %q, want /elsewhere", out.EmbeddingLocalModelDir)
	}
	if out.EmbeddingHFToken != "hf_x" {
		t.Errorf("EmbeddingHFToken = %q, want hf_x", out.EmbeddingHFToken)
	}
}

func TestMerge_LocalEmbeddingZeroKeepsBase(t *testing.T) {
	out := config.Defaults().Merge(&config.Config{})
	if out.EmbeddingLocalBackend != "auto" {
		t.Errorf("EmbeddingLocalBackend = %q, want auto", out.EmbeddingLocalBackend)
	}
	if out.EmbeddingLocalMaxSeqLen != config.MaxLocalSeqLen {
		t.Errorf("EmbeddingLocalMaxSeqLen = %d, want %d", out.EmbeddingLocalMaxSeqLen, config.MaxLocalSeqLen)
	}
	if out.EmbeddingLocalBatchSize != 32 {
		t.Errorf("EmbeddingLocalBatchSize = %d, want 32", out.EmbeddingLocalBatchSize)
	}
}

func TestValidate_LocalBackend(t *testing.T) {
	tests := []struct {
		backend string
		wantErr bool
	}{
		{"", false}, // not set; Defaults fills in "auto"
		{"auto", false},
		{"go", false},
		{"xla", false},
		{"xla:cpu", false},
		{"xla:cuda", false},
		{"metal", true},
		{"XLA", true},
		{"cuda", true},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.EmbeddingLocalBackend = tt.backend
			err := cfg.Validate()
			if tt.wantErr != (err != nil) {
				t.Fatalf("Validate() = %v, wantErr %v for backend %q", err, tt.wantErr, tt.backend)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "EmbeddingLocalBackend") {
				t.Errorf("error should name EmbeddingLocalBackend, got: %v", err)
			}
		})
	}
}

func TestValidate_LocalMaxSeqLen(t *testing.T) {
	tests := []struct {
		name    string
		value   int
		wantErr bool
	}{
		{"unset", 0, false},
		{"in range", 128, false},
		{"at maximum", config.MaxLocalSeqLen, false},
		{"negative", -1, true},
		{"above maximum", config.MaxLocalSeqLen + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.EmbeddingLocalMaxSeqLen = tt.value
			err := cfg.Validate()
			if tt.wantErr != (err != nil) {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "EmbeddingLocalMaxSeqLen") {
				t.Errorf("error should name EmbeddingLocalMaxSeqLen, got: %v", err)
			}
		})
	}
}

func TestValidate_LocalBatchSizeNegative(t *testing.T) {
	cfg := config.Defaults()
	cfg.EmbeddingLocalBatchSize = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative EmbeddingLocalBatchSize")
	}
	if !strings.Contains(err.Error(), "EmbeddingLocalBatchSize") {
		t.Errorf("error should name EmbeddingLocalBatchSize, got: %v", err)
	}
}

// TestValidate_UnknownProviderStillValidates guards the decision not to add an
// EmbeddingProvider allow-list. Only "local" is special-cased at construction
// time; every other value routes to the OpenAI-compatible client, so an
// unrecognised provider name must keep validating.
func TestValidate_UnknownProviderStillValidates(t *testing.T) {
	for _, provider := range []string{"openai", "voyage", "local", "some-future-provider"} {
		t.Run(provider, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.EmbeddingProvider = provider
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil for provider %q", err, provider)
			}
		})
	}
}

func TestModelsDir(t *testing.T) {
	dir, err := config.ModelsDir()
	if err != nil {
		t.Fatalf("ModelsDir: %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join(".codamigo", "models")) {
		t.Errorf("ModelsDir() = %q, want it to end with .codamigo/models", dir)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("ModelsDir() = %q, want an absolute path", dir)
	}
}

func TestModelsDir_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, err := config.ModelsDir(); err == nil {
		t.Fatal("expected error when home directory cannot be determined")
	}
}

// TestModelsDir_IgnoresXDGConfigHome documents the difference from
// GlobalConfigPath: downloaded weights are data, not configuration.
func TestModelsDir_IgnoresXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	dir, err := config.ModelsDir()
	if err != nil {
		t.Fatalf("ModelsDir: %v", err)
	}
	if strings.HasPrefix(dir, "/xdg") {
		t.Errorf("ModelsDir() = %q, want it to ignore XDG_CONFIG_HOME", dir)
	}
}
