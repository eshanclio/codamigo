// Package config owns the unified runtime configuration for codamigo.
//
// Configuration is loaded in four layers, with later sources winning:
// built-in defaults → global YAML file → project YAML file → environment
// variables → CLI flags. [Defaults] provides the built-in defaults; [Load]
// reads a YAML file; [LoadOrDefault] returns an empty [Config] instead of an
// error when the file is absent; [Config.Merge] applies a second [Config] on
// top of an existing one, with non-zero fields winning.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.yaml.in/yaml/v4"
)

// Config holds the unified runtime configuration for codamigo.
//
// Configuration is loaded in layers with later sources winning:
// built-in defaults → global YAML file → project YAML file → env vars → CLI flags.
//
// Duration fields are tagged yaml:"-" because they are parsed from human-readable
// strings (e.g. "500ms", "5s") via the unexported fileConfig intermediary.
type Config struct {
	// EmbeddingProvider selects the embedding backend (e.g. "openai", "voyage").
	EmbeddingProvider string `yaml:"embedding_provider"`
	// EmbeddingModel is the model name sent to the embedding API.
	EmbeddingModel string `yaml:"embedding_model"`
	// EmbeddingAPIKey authenticates requests to the embedding provider.
	EmbeddingAPIKey string `yaml:"embedding_api_key"`
	// EmbeddingBaseURL is the base URL of the embedding API (e.g. "https://api.openai.com/v1").
	EmbeddingBaseURL string `yaml:"embedding_base_url"`
	// EmbeddingDimensions is the dimensionality of the embedding vectors.
	EmbeddingDimensions int `yaml:"embedding_dimensions"`
	// EmbeddingIndexInputType is the input_type sent when embedding documents for indexing.
	EmbeddingIndexInputType string `yaml:"embedding_index_input_type"`
	// EmbeddingQueryInputType is the input_type sent when embedding queries for search.
	EmbeddingQueryInputType string `yaml:"embedding_query_input_type"`
	// EmbeddingMaxBatchSize caps the number of texts sent per embedding API call.
	EmbeddingMaxBatchSize int `yaml:"embedding_max_batch_size"`
	// EmbeddingRateLimit is the sustained requests-per-second limit for the embedding API.
	EmbeddingRateLimit float64 `yaml:"embedding_rate_limit"`
	// EmbeddingRateBurst is the maximum burst size allowed above the sustained rate.
	EmbeddingRateBurst int `yaml:"embedding_rate_burst"`
	// EmbeddingMaxRetries is the maximum number of retries on transient embedding API errors.
	EmbeddingMaxRetries int `yaml:"embedding_max_retries"`
	// EmbeddingRetryBaseDelay is the initial backoff delay before the first retry.
	EmbeddingRetryBaseDelay time.Duration `yaml:"-"`
	// EmbeddingHTTPTimeout caps each embedding HTTP request. Default 60s.
	// Increase for slow local backends; decrease for fast cloud endpoints.
	EmbeddingHTTPTimeout time.Duration `yaml:"-"`
	// IncludePatterns limits indexing to files matching these glob patterns.
	IncludePatterns []string `yaml:"include_patterns"`
	// ExcludePatterns skips files matching these glob patterns during indexing.
	ExcludePatterns []string `yaml:"exclude_patterns"`
	// StorePath is the path to the sqlite-vec database file.
	StorePath string `yaml:"store_path"`
	// ProjectRoot is the root directory to walk for source files.
	ProjectRoot string `yaml:"project_root"`
	// WatchMode controls the filesystem watcher strategy: "auto", "fsnotify", or "poll".
	WatchMode string `yaml:"watch_mode"`
	// PollInterval is the interval between poll cycles when WatchMode is "poll".
	PollInterval time.Duration `yaml:"-"`
	// DebounceWindow groups rapid filesystem events into a single re-index pass.
	DebounceWindow time.Duration `yaml:"-"`
	// IndexConcurrency is the maximum number of files indexed concurrently.
	IndexConcurrency int `yaml:"index_concurrency"`
	// MaxFileSize is the maximum file size in bytes to index. Files larger than
	// this are skipped. 0 means no limit. The default (set by [Defaults]) is 1 MB.
	MaxFileSize int64 `yaml:"-"`
	// WriteBatchSize is the number of files per DB write transaction during
	// batch indexing. 0 means use the default (50).
	WriteBatchSize int `yaml:"write_batch_size"`
	// NonCodeLanguages lists language names excluded from the map when
	// CodeOnly is true. Defaults to ["markdown", "yaml", "json"].
	NonCodeLanguages []string `yaml:"non_code_languages"`
}

// fileConfig is the YAML-deserializable form of Config. Duration fields are
// strings so users write "500ms" or "5s" instead of raw nanosecond integers.
type fileConfig struct {
	EmbeddingProvider       string   `yaml:"embedding_provider"`
	EmbeddingModel          string   `yaml:"embedding_model"`
	EmbeddingAPIKey         string   `yaml:"embedding_api_key"`
	EmbeddingBaseURL        string   `yaml:"embedding_base_url"`
	EmbeddingDimensions     int      `yaml:"embedding_dimensions"`
	EmbeddingIndexInputType string   `yaml:"embedding_index_input_type"`
	EmbeddingQueryInputType string   `yaml:"embedding_query_input_type"`
	EmbeddingMaxBatchSize   int      `yaml:"embedding_max_batch_size"`
	EmbeddingRateLimit      float64  `yaml:"embedding_rate_limit"`
	EmbeddingRateBurst      int      `yaml:"embedding_rate_burst"`
	EmbeddingMaxRetries     int      `yaml:"embedding_max_retries"`
	EmbeddingRetryBaseDelay string   `yaml:"embedding_retry_base_delay"`
	EmbeddingHTTPTimeout    string   `yaml:"embedding_http_timeout"`
	IncludePatterns         []string `yaml:"include_patterns"`
	ExcludePatterns         []string `yaml:"exclude_patterns"`
	StorePath               string   `yaml:"store_path"`
	ProjectRoot             string   `yaml:"project_root"`
	WatchMode               string   `yaml:"watch_mode"`
	PollInterval            string   `yaml:"poll_interval"`
	DebounceWindow          string   `yaml:"debounce_window"`
	IndexConcurrency        int      `yaml:"index_concurrency"`
	MaxFileSize             *int64   `yaml:"max_file_size,omitempty"`
	WriteBatchSize          int      `yaml:"write_batch_size"`
	NonCodeLanguages        []string `yaml:"non_code_languages"`
}

// maxFileSizeFromYAML converts the optional YAML pointer to an int64 for Config.
// nil (absent from YAML) → 0 (merge treats as "not set", keeps default).
// *int64(0) (explicit YAML "max_file_size: 0") → -1 (sentinel: merge sets to 0 = no limit).
// *int64(N) (explicit positive value) → N.
func maxFileSizeFromYAML(p *int64) int64 {
	if p == nil {
		return 0
	}
	if *p == 0 {
		return -1
	}
	return *p
}

func parseDuration(field, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", field, value, err)
	}
	return d, nil
}

func (fc *fileConfig) toConfig() (*Config, error) {
	retryDelay, err := parseDuration("embedding_retry_base_delay", fc.EmbeddingRetryBaseDelay)
	if err != nil {
		return nil, err
	}
	httpTimeout, err := parseDuration("embedding_http_timeout", fc.EmbeddingHTTPTimeout)
	if err != nil {
		return nil, err
	}
	pollInterval, err := parseDuration("poll_interval", fc.PollInterval)
	if err != nil {
		return nil, err
	}
	debounceWindow, err := parseDuration("debounce_window", fc.DebounceWindow)
	if err != nil {
		return nil, err
	}
	return &Config{
		EmbeddingProvider:       fc.EmbeddingProvider,
		EmbeddingModel:          fc.EmbeddingModel,
		EmbeddingAPIKey:         fc.EmbeddingAPIKey,
		EmbeddingBaseURL:        fc.EmbeddingBaseURL,
		EmbeddingDimensions:     fc.EmbeddingDimensions,
		EmbeddingIndexInputType: fc.EmbeddingIndexInputType,
		EmbeddingQueryInputType: fc.EmbeddingQueryInputType,
		EmbeddingMaxBatchSize:   fc.EmbeddingMaxBatchSize,
		EmbeddingRateLimit:      fc.EmbeddingRateLimit,
		EmbeddingRateBurst:      fc.EmbeddingRateBurst,
		EmbeddingMaxRetries:     fc.EmbeddingMaxRetries,
		EmbeddingRetryBaseDelay: retryDelay,
		EmbeddingHTTPTimeout:    httpTimeout,
		IncludePatterns:         fc.IncludePatterns,
		ExcludePatterns:         fc.ExcludePatterns,
		StorePath:               fc.StorePath,
		ProjectRoot:             fc.ProjectRoot,
		WatchMode:               fc.WatchMode,
		PollInterval:            pollInterval,
		DebounceWindow:          debounceWindow,
		IndexConcurrency:        fc.IndexConcurrency,
		MaxFileSize:             maxFileSizeFromYAML(fc.MaxFileSize),
		WriteBatchSize:          fc.WriteBatchSize,
		NonCodeLanguages:        fc.NonCodeLanguages,
	}, nil
}

// Defaults returns a Config populated with built-in default values.
// Use as the starting point before merging file and flag configs.
func Defaults() *Config {
	return &Config{
		EmbeddingProvider:       "openai",
		EmbeddingModel:          "text-embedding-3-small",
		EmbeddingBaseURL:        "https://api.openai.com/v1",
		EmbeddingDimensions:     1536,
		EmbeddingMaxBatchSize:   256,
		EmbeddingRateLimit:      500,
		EmbeddingRateBurst:      100,
		EmbeddingMaxRetries:     3,
		EmbeddingRetryBaseDelay: 500 * time.Millisecond,
		EmbeddingHTTPTimeout:    60 * time.Second,
		StorePath:               ".codamigo/store.db",
		WatchMode:               "auto",
		PollInterval:            5 * time.Second,
		DebounceWindow:          500 * time.Millisecond,
		IndexConcurrency:        20,
		MaxFileSize:             1_048_576, // 1 MB
		WriteBatchSize:          50,
		NonCodeLanguages:        []string{"markdown", "yaml", "json"},
	}
}

// GlobalConfigPath returns the path to the user-global config file.
// Honours XDG_CONFIG_HOME when set; falls back to ~/.codamigo/global_settings.yml.
// Returns an error when the home directory cannot be determined.
func GlobalConfigPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("XDG_CONFIG_HOME must be an absolute path, got %q", dir)
		}
		return filepath.Join(dir, "codamigo", "global_settings.yml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}
	return filepath.Join(home, ".codamigo", "global_settings.yml"), nil
}

// ProjectConfigPath returns the path to the per-project config file.
func ProjectConfigPath() string {
	return filepath.Join(".codamigo", "settings.yml")
}

// Load reads a Config from a YAML file at path.
// Returns an error if the file does not exist, contains unknown keys, or has
// malformed duration strings.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening config %q: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var fc fileConfig
	if err := dec.Decode(&fc); err != nil {
		if errors.Is(err, io.EOF) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}
	return fc.toConfig()
}

// LoadOrDefault reads a Config from path, returning a zero-value Config if the
// file does not exist.
func LoadOrDefault(path string) (*Config, error) {
	cfg, err := Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	return cfg, err
}

// Merge returns a new Config where non-zero fields in overlay override c.
// Zero-value fields in the overlay are treated as "not set" and do not
// override the base value. This means it is impossible to explicitly set
// numeric fields (like EmbeddingMaxRetries) to 0 via an overlay config.
// For slices, a non-nil overlay slice (even empty) overrides the base value,
// allowing an explicit empty list in YAML to clear inherited patterns.
func (c *Config) Merge(o *Config) *Config {
	out := *c
	if o.EmbeddingProvider != "" {
		out.EmbeddingProvider = o.EmbeddingProvider
	}
	if o.EmbeddingModel != "" {
		out.EmbeddingModel = o.EmbeddingModel
	}
	if o.EmbeddingAPIKey != "" {
		out.EmbeddingAPIKey = o.EmbeddingAPIKey
	}
	if o.EmbeddingBaseURL != "" {
		out.EmbeddingBaseURL = o.EmbeddingBaseURL
	}
	if o.EmbeddingDimensions != 0 {
		out.EmbeddingDimensions = o.EmbeddingDimensions
	}
	if o.EmbeddingIndexInputType != "" {
		out.EmbeddingIndexInputType = o.EmbeddingIndexInputType
	}
	if o.EmbeddingQueryInputType != "" {
		out.EmbeddingQueryInputType = o.EmbeddingQueryInputType
	}
	if o.EmbeddingMaxBatchSize != 0 {
		out.EmbeddingMaxBatchSize = o.EmbeddingMaxBatchSize
	}
	if o.EmbeddingRateLimit != 0 {
		out.EmbeddingRateLimit = o.EmbeddingRateLimit
	}
	if o.EmbeddingRateBurst != 0 {
		out.EmbeddingRateBurst = o.EmbeddingRateBurst
	}
	if o.EmbeddingMaxRetries != 0 {
		out.EmbeddingMaxRetries = o.EmbeddingMaxRetries
	}
	if o.EmbeddingRetryBaseDelay != 0 {
		out.EmbeddingRetryBaseDelay = o.EmbeddingRetryBaseDelay
	}
	if o.EmbeddingHTTPTimeout != 0 {
		out.EmbeddingHTTPTimeout = o.EmbeddingHTTPTimeout
	}
	if o.IncludePatterns != nil {
		out.IncludePatterns = slices.Clone(o.IncludePatterns)
	}
	if o.ExcludePatterns != nil {
		out.ExcludePatterns = slices.Clone(o.ExcludePatterns)
	}
	if o.StorePath != "" {
		out.StorePath = o.StorePath
	}
	if o.ProjectRoot != "" {
		out.ProjectRoot = o.ProjectRoot
	}
	if o.WatchMode != "" {
		out.WatchMode = o.WatchMode
	}
	if o.PollInterval != 0 {
		out.PollInterval = o.PollInterval
	}
	if o.DebounceWindow != 0 {
		out.DebounceWindow = o.DebounceWindow
	}
	if o.IndexConcurrency != 0 {
		out.IndexConcurrency = o.IndexConcurrency
	}
	if o.MaxFileSize < 0 {
		out.MaxFileSize = 0 // negative in overlay means "no limit"
	} else if o.MaxFileSize > 0 {
		out.MaxFileSize = o.MaxFileSize
	}
	if o.WriteBatchSize != 0 {
		out.WriteBatchSize = o.WriteBatchSize
	}
	if o.NonCodeLanguages != nil {
		out.NonCodeLanguages = slices.Clone(o.NonCodeLanguages)
	}
	return &out
}

// Validate checks all config fields for invalid values and returns a joined
// error listing every violation found. Returns nil when the config is valid.
func (c *Config) Validate() error {
	var errs []error
	if c.EmbeddingDimensions < 0 {
		errs = append(errs, errors.New("EmbeddingDimensions must not be negative"))
	} else if c.EmbeddingModel != "" && c.EmbeddingDimensions == 0 {
		errs = append(errs, errors.New("EmbeddingDimensions must be > 0 when EmbeddingModel is set"))
	}
	if c.WatchMode != "" && c.WatchMode != "auto" && c.WatchMode != "fsnotify" && c.WatchMode != "poll" {
		errs = append(errs, fmt.Errorf("WatchMode %q is not valid (use auto, fsnotify, or poll)", c.WatchMode))
	}
	if c.EmbeddingModel != "" && c.EmbeddingProvider == "" {
		errs = append(errs, errors.New("EmbeddingProvider must be set when EmbeddingModel is set"))
	}
	if c.IndexConcurrency < 0 {
		errs = append(errs, errors.New("IndexConcurrency must not be negative"))
	}
	if c.MaxFileSize < 0 {
		errs = append(errs, errors.New("MaxFileSize must not be negative"))
	}
	if c.WriteBatchSize < 0 {
		errs = append(errs, errors.New("WriteBatchSize must not be negative"))
	}
	if c.PollInterval < 0 {
		errs = append(errs, errors.New("PollInterval must not be negative"))
	}
	if c.DebounceWindow < 0 {
		errs = append(errs, errors.New("DebounceWindow must not be negative"))
	}
	if c.EmbeddingRetryBaseDelay < 0 {
		errs = append(errs, errors.New("EmbeddingRetryBaseDelay must not be negative"))
	}
	if c.EmbeddingHTTPTimeout < 0 {
		errs = append(errs, errors.New("EmbeddingHTTPTimeout must not be negative"))
	}
	if c.EmbeddingBaseURL != "" {
		u, err := url.ParseRequestURI(c.EmbeddingBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("EmbeddingBaseURL %q is not a valid HTTP(S) URL", c.EmbeddingBaseURL))
		}
	}
	return errors.Join(errs...)
}
