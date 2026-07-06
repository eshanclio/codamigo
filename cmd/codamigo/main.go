package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/embedder/openaicompat"
	"github.com/ieshan/codamigo/indexer"
	"github.com/ieshan/codamigo/mcp"
	"github.com/ieshan/codamigo/query"
	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/codamigo/walker"
	"github.com/ieshan/codamigo/watcher"
	"github.com/ieshan/go-code-chunker/chunker"
	"github.com/ieshan/go-code-chunker/langs"
)

// commonFlags are included in every subcommand. Each flag reads its value from
// the corresponding CODAMIGO_* environment variable when not set explicitly.
var commonFlags = []cli.Flag{
	&cli.StringFlag{
		Name:    "api-key",
		Usage:   "embedding API key (optional; required for remote providers)",
		Sources: cli.EnvVars("CODAMIGO_API_KEY"),
	},
	&cli.StringFlag{
		Name:    "model",
		Usage:   "embedding model name",
		Sources: cli.EnvVars("CODAMIGO_MODEL"),
	},
	&cli.StringFlag{
		Name:    "base-url",
		Usage:   "embedding API base URL",
		Sources: cli.EnvVars("CODAMIGO_BASE_URL"),
	},
	&cli.StringFlag{
		Name:    "store-path",
		Usage:   "path to SQLite store file",
		Sources: cli.EnvVars("CODAMIGO_STORE_PATH"),
	},
	&cli.StringFlag{
		Name:    "project-root",
		Usage:   "project root directory to index",
		Sources: cli.EnvVars("CODAMIGO_PROJECT_ROOT"),
	},
	&cli.IntFlag{
		Name:    "dimensions",
		Usage:   "embedding vector dimensions",
		Sources: cli.EnvVars("CODAMIGO_DIMENSIONS"),
	},
	&cli.StringFlag{
		Name:    "global-config",
		Usage:   "path to global config file (default: ~/.codamigo/global_settings.yml)",
		Sources: cli.EnvVars("CODAMIGO_GLOBAL_CONFIG"),
	},
	&cli.StringFlag{
		Name:    "project-config",
		Usage:   "path to project config file (default: .codamigo/settings.yml)",
		Sources: cli.EnvVars("CODAMIGO_PROJECT_CONFIG"),
	},
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cmd := &cli.Command{
		Name:  "codamigo",
		Usage: "semantic code search via tree-sitter and embeddings",
		Commands: []*cli.Command{
			initCmd(),
			indexCmd(),
			searchCmd(),
			mapCmd(),
			serveCmd(),
			resetCmd(),
			doctorCmd(),
		},
	}
	if err := cmd.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "codamigo: %v\n", err)
		os.Exit(1)
	}
}

func indexCmd() *cli.Command {
	return &cli.Command{
		Name:  "index",
		Usage: "walk the project, embed source chunks, and store them",
		Flags: commonFlags,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			c, s, w, err := buildComponents(cfg)
			if err != nil {
				return err
			}
			defer s.Close()
			defer w.Close() //nolint:errcheck

			emb, err := newEmbedder(cfg, cfg.EmbeddingIndexInputType)
			if err != nil {
				return fmt.Errorf("creating embedder: %w", err)
			}

			// Wrap ctx so the TUI can cancel the indexer directly via ctrl+c.
			// In TTY mode the terminal is in raw mode (ISIG cleared), so ctrl+c
			// arrives as a KeyPressMsg rather than SIGINT.
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			prog, reporter := newProgressTUI(cancel)

			// Explicit interface conversion avoids the non-nil interface / nil pointer
			// trap: passing (*progressReporter)(nil) directly as indexer.Progress produces
			// a non-nil interface whose nil guard passes but whose method calls panic.
			var progress indexer.Progress
			if reporter != nil {
				progress = reporter
			}

			idx := indexer.New(c, emb, s, w, cfg.IndexConcurrency, cfg.MaxFileSize, cfg.WriteBatchSize, nil, progress)

			printFailedFiles := func() {
				if reporter == nil {
					return
				}
				if failed := reporter.FailedFiles(); len(failed) > 0 {
					fmt.Fprintf(os.Stderr, "codamigo: %d file(s) failed to index:\n", len(failed))
					for _, p := range failed {
						fmt.Fprintf(os.Stderr, "  %s\n", p)
					}
					fmt.Fprintln(os.Stderr, "These files will be retried on the next index run.")
				}
			}

			if prog == nil {
				err = idx.Index(ctx)
				printFailedFiles()
				return err
			}

			// errCh carries the indexer result. Buffered capacity 1 so the goroutine
			// never blocks even if prog.Run() returns an error early.
			errCh := make(chan error, 1)
			tickDone := make(chan struct{})

			go func() {
				err = idx.Index(ctx)
				errCh <- err    // send before closing tickDone
				close(tickDone) // stop the ticker goroutine
				prog.Send(indexDoneMsg{
					processed: reporter.processed.Load(),
					skipped:   reporter.skipped.Load(),
				})
			}()

			go runTicker(prog, reporter, tickDone)

			// _ is intentional: the final model state is not needed post-exit.
			_, runErr := prog.Run()

			// Always drain errCh — ensures the indexer goroutine has fully exited.
			indexErr := <-errCh

			printFailedFiles()

			if runErr != nil {
				return runErr
			}
			return indexErr
		},
	}
}

func serveCmd() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "start MCP stdio server",
		Flags: commonFlags,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			c, s, w, err := buildComponents(cfg)
			if err != nil {
				return err
			}
			defer s.Close()
			defer w.Close()
			indexEmb, err := newEmbedder(cfg, cfg.EmbeddingIndexInputType)
			if err != nil {
				return fmt.Errorf("creating embedder: %w", err)
			}
			queryEmb, err := newEmbedder(cfg, cfg.EmbeddingQueryInputType)
			if err != nil {
				return fmt.Errorf("creating embedder: %w", err)
			}
			q := query.New(queryEmb, s)
			idx := indexer.New(c, indexEmb, s, w, cfg.IndexConcurrency, cfg.MaxFileSize, cfg.WriteBatchSize, q.InvalidateMapCache, nil)
			wch, err := watcher.New(cfg, w.Match, w.FS())
			if err != nil {
				return fmt.Errorf("creating watcher: %w", err)
			}
			defer wch.Close()
			srv := mcp.NewServer(q, idx, wch, cfg.NonCodeLanguages)
			return srv.Serve(ctx)
		},
	}
}

// loadConfig assembles the final Config by merging four layers in priority order:
// built-in defaults → global file → project file → CLI flags (which absorb env vars).
func loadConfig(cmd *cli.Command) (*config.Config, error) {
	globalPath, err := config.GlobalConfigPath()
	if err != nil {
		return nil, fmt.Errorf("resolving global config path: %w", err)
	}
	if cmd.IsSet("global-config") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("determining home directory: %w", err)
		}
		globalPath, err = safeConfigPath(home, cmd.String("global-config"))
		if err != nil {
			return nil, fmt.Errorf("global config: %w", err)
		}
	}
	projectPath := config.ProjectConfigPath()
	if cmd.IsSet("project-config") {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determining working directory: %w", err)
		}
		projectPath, err = safeConfigPath(wd, cmd.String("project-config"))
		if err != nil {
			return nil, fmt.Errorf("project config: %w", err)
		}
	}

	cfg := config.Defaults()

	globalCfg, err := config.LoadOrDefault(globalPath)
	if err != nil {
		return nil, fmt.Errorf("global config: %w", err)
	}
	cfg = cfg.Merge(globalCfg)

	projectCfg, err := config.LoadOrDefault(projectPath)
	if err != nil {
		return nil, fmt.Errorf("project config: %w", err)
	}
	cfg = cfg.Merge(projectCfg)

	cfg = cfg.Merge(flagsToConfig(cmd))

	if cfg.ProjectRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determining project root: %w", err)
		}
		cfg.ProjectRoot = wd
	}

	if err = cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

// flagsToConfig converts CLI flag values into a sparse Config. Only fields
// explicitly set by the user (via flag or env var) are populated; zero fields
// are ignored by Merge so they don't override file-loaded values.
func flagsToConfig(cmd *cli.Command) *config.Config {
	cfg := &config.Config{}
	if cmd.IsSet("api-key") {
		cfg.EmbeddingAPIKey = cmd.String("api-key")
	}
	if cmd.IsSet("model") {
		cfg.EmbeddingModel = cmd.String("model")
	}
	if cmd.IsSet("base-url") {
		cfg.EmbeddingBaseURL = cmd.String("base-url")
	}
	if cmd.IsSet("store-path") {
		cfg.StorePath = cmd.String("store-path")
	}
	if cmd.IsSet("project-root") {
		cfg.ProjectRoot = cmd.String("project-root")
	}
	if cmd.IsSet("dimensions") {
		cfg.EmbeddingDimensions = cmd.Int("dimensions")
	}
	return cfg
}

func buildStore(cfg *config.Config) (store.Store, error) {
	s, err := store.NewSQLiteStore(cfg.StorePath, cfg.EmbeddingModel, cfg.EmbeddingDimensions)
	if err != nil {
		return nil, fmt.Errorf("opening store: %w", err)
	}
	return s, nil
}

// buildExtensionFilter returns a predicate that accepts only files whose
// lowercase extension appears in the supported-language set derived from langs.
// Files with no extension are rejected. The returned closure is safe for
// concurrent use — the underlying map is immutable after construction.
func buildExtensionFilter(langConfigs []chunker.LanguageConfig) func(string) bool {
	total := 0
	for _, lang := range langConfigs {
		total += len(lang.Extensions)
	}
	exts := make(map[string]struct{}, total)
	for _, lang := range langConfigs {
		for _, ext := range lang.Extensions {
			exts[strings.ToLower(ext)] = struct{}{}
		}
	}
	return func(path string) bool {
		ext := strings.ToLower(filepath.Ext(path))
		if ext == "" {
			return false
		}
		_, ok := exts[ext]
		return ok
	}
}

func buildComponents(cfg *config.Config) (*chunker.Chunker, store.Store, *walker.Walker, error) {
	allLangs := langs.AllLanguages()
	c, err := chunker.NewChunker(allLangs, chunker.DefaultConfig())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating chunker: %w", err)
	}
	s, err := store.NewSQLiteStore(cfg.StorePath, cfg.EmbeddingModel, cfg.EmbeddingDimensions)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening store: %w", err)
	}
	filter := buildExtensionFilter(allLangs)
	w, err := walker.New(cfg.ProjectRoot, cfg, walker.WithFileFilter(filter))
	if err != nil {
		s.Close()
		return nil, nil, nil, fmt.Errorf("creating walker: %w", err)
	}
	return c, s, w, nil
}

func newEmbedder(cfg *config.Config, inputType string) (*openaicompat.Client, error) {
	return openaicompat.New(openaicompat.Options{
		BaseURL:        cfg.EmbeddingBaseURL,
		APIKey:         cfg.EmbeddingAPIKey,
		Model:          cfg.EmbeddingModel,
		MaxBatchSize:   cfg.EmbeddingMaxBatchSize,
		Dimensions:     cfg.EmbeddingDimensions,
		InputType:      inputType,
		RateLimit:      cfg.EmbeddingRateLimit,
		RateBurst:      cfg.EmbeddingRateBurst,
		MaxRetries:     cfg.EmbeddingMaxRetries,
		RetryBaseDelay: cfg.EmbeddingRetryBaseDelay,
		Concurrency:    cfg.IndexConcurrency,
		HTTPTimeout:    cfg.EmbeddingHTTPTimeout,
	})
}

// safeConfigPath validates that userPath does not escape the base directory.
func safeConfigPath(base, userPath string) (string, error) {
	abs, err := filepath.Abs(userPath)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolving base: %w", err)
	}
	rel, err := filepath.Rel(baseAbs, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("config path %q escapes base directory %q", userPath, base)
	}
	return abs, nil
}
