package localembed

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
)

// The candidate order is the fallback contract, so it is asserted directly.
// "auto" must try XLA before the pure-Go backend: Go is 12-25x slower and
// unusable for indexing, so a silent reordering would look like a performance
// regression rather than a bug.
func TestBackendConfigs(t *testing.T) {
	tests := []struct {
		cfg     string
		want    []string
		wantErr bool
	}{
		{cfg: "", want: []string{"xla:cuda", "xla", "go"}},
		{cfg: "auto", want: []string{"xla:cuda", "xla", "go"}},
		{cfg: "go", want: []string{"go"}},
		{cfg: "xla", want: []string{"xla"}},
		{cfg: "xla:cpu", want: []string{"xla:cpu"}},
		{cfg: "xla:cuda", want: []string{"xla:cuda"}},
		{cfg: "metal", wantErr: true},
		{cfg: "cuda", wantErr: true},
		{cfg: "XLA", wantErr: true},
		{cfg: "AUTO", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.cfg, func(t *testing.T) {
			got, err := backendConfigs(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("backendConfigs(%q) = %v, want error", tt.cfg, got)
				}
				if !errors.Is(err, ErrUnsupportedBackend) {
					t.Errorf("error should wrap ErrUnsupportedBackend, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("backendConfigs(%q): %v", tt.cfg, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("backendConfigs(%q) = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}

func TestSelectBackend_InvalidConfig(t *testing.T) {
	backend, name, err := selectBackend("nonsense")
	if err == nil {
		backend.Finalize()
		t.Fatalf("selectBackend(nonsense) = %q, want error", name)
	}
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Errorf("error should wrap ErrUnsupportedBackend, got: %v", err)
	}
}

// TestSelectBackend_Go asserts the always-available fallback really is
// available, so "auto" can never fail outright on a supported platform.
func TestSelectBackend_Go(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	backend, name, err := selectBackend("go")
	if err != nil {
		t.Fatalf("selectBackend(go): %v", err)
	}
	defer backend.Finalize()
	if name != "go" {
		t.Errorf("selected backend = %q, want go", name)
	}
}
