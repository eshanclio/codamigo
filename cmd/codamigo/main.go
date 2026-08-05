package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/indexer"
	"github.com/ieshan/codamigo/localembed"
	"github.com/ieshan/codamigo/mcp"
	"github.com/ieshan/codamigo/query"
	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/codamigo/walker"
	"github.com/ieshan/codamigo/watcher"
	"github.com/ieshan/go-code-chunker/chunker"
	"github.com/ieshan/go-code-chunker/langs"
	"github.com/ieshan/go-embedder"
	"github.com/ieshan/go-embedder/openai"
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
		Name:    "provider",
		Usage:   "embedding provider (\"local\" runs in-process; anything else is an OpenAI-compatible API)",
		Sources: cli.EnvVars("CODAMIGO_PROVIDER"),
	},
	&cli.StringFlag{
		Name:    "model",
		Usage:   "embedding model name",
		Sources: cli.EnvVars("CODAMIGO_MODEL"),
	},
	&cli.StringFlag{
		Name:  "hf-token",
		Usage: "HuggingFace token, only needed for gated or private models",
		// HF_TOKEN is the conventional name, so honour it as a fallback.
		Sources: cli.EnvVars("CODAMIGO_HF_TOKEN", "HF_TOKEN"),
	},
	&cli.StringFlag{
		Name:    "base-url",
		Usage:   "embedding API base URL",
		Sources: cli.EnvVars("CODAMIGO_BASE_URL"),
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
		Usage:   "path to project config file (overrides default lookup)",
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
			callersCmd(),
			calleesCmd(),
			impactCmd(),
			serveCmd(),
			resetCmd(),
			doctorCmd(),
			downloadModelCmd(),
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
			emb, err := newEmbedder(cfg, roleDocument)
			if err != nil {
				return fmt.Errorf("creating embedder: %w", err)
			}
			defer closeEmbedder(emb)
			storePath, err := config.DefaultStorePath(cfg.ProjectRoot)
			if err != nil {
				return fmt.Errorf("resolving store path: %w", err)
			}
			c, s, w, err := buildComponents(cfg, storePath, emb.Dim())
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }() // best-effort cleanup; the process is exiting either way
			defer func() { _ = w.Close() }() // best-effort cleanup; the process is exiting either way

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

			idx := indexer.New(c, emb, s, w,
				indexer.WithConcurrency(cfg.IndexConcurrency),
				indexer.WithMaxFileSize(cfg.MaxFileSize),
				indexer.WithWriteBatchSize(cfg.WriteBatchSize),
				indexer.WithProgress(progress),
				indexer.WithGraph(cfg.GraphEnabled()))

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
			indexEmb, err := newEmbedder(cfg, roleDocument)
			if err != nil {
				return fmt.Errorf("creating embedder: %w", err)
			}
			// indexEmb owns the model; queryEmb below is a view over it, so
			// closing indexEmb drains queries too.
			defer closeEmbedder(indexEmb)
			storePath, err := config.DefaultStorePath(cfg.ProjectRoot)
			if err != nil {
				return fmt.Errorf("resolving store path: %w", err)
			}
			c, s, w, err := buildComponents(cfg, storePath, indexEmb.Dim())
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }() // best-effort cleanup; the process is exiting either way
			defer func() { _ = w.Close() }() // best-effort cleanup; the process is exiting either way
			queryEmb, err := queryEmbedderFor(cfg, indexEmb)
			if err != nil {
				return fmt.Errorf("creating embedder: %w", err)
			}
			defer closeEmbedder(queryEmb)
			q := query.New(queryEmb, s)
			idx := indexer.New(c, indexEmb, s, w,
				indexer.WithConcurrency(cfg.IndexConcurrency),
				indexer.WithMaxFileSize(cfg.MaxFileSize),
				indexer.WithWriteBatchSize(cfg.WriteBatchSize),
				indexer.WithOnIndexed(q.InvalidateMapCache),
				indexer.WithGraph(cfg.GraphEnabled()))
			wch, err := watcher.New(cfg, w.Match, w.FS())
			if err != nil {
				return fmt.Errorf("creating watcher: %w", err)
			}
			defer func() { _ = wch.Close() }() // best-effort cleanup; the process is exiting either way
			srv := mcp.NewServer(q, idx, wch,
				mcp.WithNonCodeLanguages(cfg.NonCodeLanguages),
				mcp.WithGraph(cfg.GraphEnabled()),
				mcp.WithStaleRefreshThreshold(cfg.StaleRefreshThreshold))
			return srv.Serve(ctx)
		},
	}
}

// loadConfig assembles the final Config by merging five layers in priority order:
// built-in defaults → global file → home project file → in-project file → CLI flags.
// The home project config is loaded from ~/.codamigo/projects/<hash>/settings.yml;
// if absent, the in-project .codamigo/settings.yml is used as fallback.
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

	cfg := config.Defaults()

	globalCfg, err := config.LoadOrDefault(globalPath)
	if err != nil {
		return nil, fmt.Errorf("global config: %w", err)
	}
	cfg = cfg.Merge(globalCfg)

	// Compute flags once — reused for early ProjectRoot resolution and final merge.
	flagCfg := flagsToConfig(cmd)

	// Resolve ProjectRoot early — needed for HomeProjectConfigPath.
	// project_root in a project config would be circular, so we only
	// consider defaults, global config, and flags here.
	if flagCfg.ProjectRoot != "" {
		cfg.ProjectRoot = flagCfg.ProjectRoot
	}
	if cfg.ProjectRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determining project root: %w", err)
		}
		cfg.ProjectRoot = wd
	}

	// Load project config: home first, fallback to in-project.
	// --project-config overrides both layers.
	var projectCfg *config.Config
	if cmd.IsSet("project-config") {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determining working directory: %w", err)
		}
		projectPath, err := safeConfigPath(wd, cmd.String("project-config"))
		if err != nil {
			return nil, fmt.Errorf("project config: %w", err)
		}
		projectCfg, err = config.LoadOrDefault(projectPath)
		if err != nil {
			return nil, fmt.Errorf("project config: %w", err)
		}
	} else {
		homePath, err := config.HomeProjectConfigPath(cfg.ProjectRoot)
		if err != nil {
			return nil, fmt.Errorf("resolving home project config: %w", err)
		}
		projectCfg, err = config.Load(homePath)
		if errors.Is(err, fs.ErrNotExist) {
			projectCfg, err = config.LoadOrDefault(config.ProjectConfigPath())
		}
		if err != nil {
			return nil, fmt.Errorf("project config: %w", err)
		}
	}
	cfg = cfg.Merge(projectCfg)

	// Merge flags last so flags win over project config.
	cfg = cfg.Merge(flagCfg)

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
	if cmd.IsSet("provider") {
		cfg.EmbeddingProvider = cmd.String("provider")
	}
	if cmd.IsSet("hf-token") {
		cfg.EmbeddingHFToken = cmd.String("hf-token")
	}
	if cmd.IsSet("model") {
		cfg.EmbeddingModel = cmd.String("model")
	}
	if cmd.IsSet("base-url") {
		cfg.EmbeddingBaseURL = cmd.String("base-url")
	}
	if cmd.IsSet("project-root") {
		cfg.ProjectRoot = cmd.String("project-root")
	}
	if cmd.IsSet("dimensions") {
		cfg.EmbeddingDimensions = cmd.Int("dimensions")
	}
	return cfg
}

func buildStore(storePath string, embeddingModel string, dim int) (store.Store, error) {
	s, err := store.NewSQLiteStore(storePath, embeddingModel, dim)
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

func buildComponents(cfg *config.Config, storePath string, dim int) (*chunker.Chunker, store.Store, *walker.Walker, error) {
	allLangs := langs.AllLanguages()
	c, err := chunker.NewChunker(allLangs, chunker.DefaultConfig())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating chunker: %w", err)
	}
	s, err := store.NewSQLiteStore(storePath, cfg.EmbeddingModel, dim)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening store: %w", err)
	}
	filter := buildExtensionFilter(allLangs)
	w, err := walker.New(cfg.ProjectRoot, cfg, walker.WithFileFilter(filter))
	if err != nil {
		_ = s.Close() // best-effort cleanup; the walker error below is the one worth reporting
		return nil, nil, nil, fmt.Errorf("creating walker: %w", err)
	}
	return c, s, w, nil
}

// localProvider is the one embedding_provider value that is special-cased.
// Every other value — including "voyage" — routes to the OpenAI-compatible
// client, so adding a provider name never requires a code change here.
const localProvider = "local"

// embedderRole says which side of the index/query split an embedder serves.
//
// It is an explicit parameter rather than being inferred from the input-type
// string, because both embedding_index_input_type and
// embedding_query_input_type default to empty and would be indistinguishable.
type embedderRole int

const (
	roleDocument embedderRole = iota
	roleQuery
)

// newEmbedder builds the embedder for cfg's provider.
//
// The return type is the interface, so the local provider can be substituted
// without touching any call site: they only use Dim() and the Embed methods, and
// query.New/indexer.New already take embedder.Embedder.
//
// Callers must pair this with closeEmbedder — the local provider holds a compute
// backend and compiled graphs that are not garbage-collected.
func newEmbedder(cfg *config.Config, role embedderRole) (embedder.Embedder, error) {
	if cfg.EmbeddingProvider == localProvider {
		return newLocalEmbedder(cfg, role)
	}
	inputType := cfg.EmbeddingIndexInputType
	if role == roleQuery {
		inputType = cfg.EmbeddingQueryInputType
	}
	client, err := openai.New(openai.Options{
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
	if err != nil {
		// Return an untyped nil, not the typed nil *openai.Client: `return
		// openai.New(...)` would hand back a non-nil interface wrapping a nil
		// pointer, so a caller's `emb != nil` guard would pass and then panic on
		// the first method call. Same trap as the Progress conversion in indexCmd.
		return nil, err
	}
	return client, nil
}

// newLocalEmbedder constructs the in-process GoMLX embedder, applying the
// model's query instruction prefix on the query side only.
func newLocalEmbedder(cfg *config.Config, role embedderRole) (embedder.Embedder, error) {
	root, err := localModelsRoot(cfg)
	if err != nil {
		return nil, err
	}
	emb, err := localembed.New(localembed.Options{
		Model:      cfg.EmbeddingModel,
		ModelsRoot: root,
		Backend:    cfg.EmbeddingLocalBackend,
		MaxSeqLen:  cfg.EmbeddingLocalMaxSeqLen,
		BatchSize:  cfg.EmbeddingLocalBatchSize,
		Dimensions: cfg.EmbeddingDimensions,
		// ApplyQueryPrefix rather than WithPrefix: a WithPrefix view does not own
		// Close, so returning one here would leave the caller's
		// defer closeEmbedder a no-op and the compute backend never finalized.
		ApplyQueryPrefix: role == roleQuery,
	})
	if err != nil {
		return nil, err // untyped nil: see the note in newEmbedder
	}
	return emb, nil
}

// localModelsRoot resolves where downloaded models live: the configured
// override, else $HOME/.codamigo/models.
func localModelsRoot(cfg *config.Config) (string, error) {
	if cfg.EmbeddingLocalModelDir != "" {
		abs, err := filepath.Abs(cfg.EmbeddingLocalModelDir)
		if err != nil {
			return "", fmt.Errorf("resolving embedding_local_model_dir: %w", err)
		}
		return abs, nil
	}
	root, err := config.ModelsDir()
	if err != nil {
		return "", fmt.Errorf("resolving models directory: %w", err)
	}
	return root, nil
}

// closeEmbedder releases native resources held by embedders that own them.
// The local GoMLX embedder holds a compute backend and compiled graphs; the
// HTTP embedder holds nothing, so this is a no-op for it.
func closeEmbedder(e embedder.Embedder) {
	if c, ok := e.(io.Closer); ok {
		if err := c.Close(); err != nil {
			slog.Warn("closing embedder", slog.Any("error", err))
		}
	}
}

// queryEmbedderFor returns the query-side embedder to pair with a document-side
// one that has already been built.
//
// For the local provider this is a view over the same weights, which is what
// keeps `serve` from holding two copies of the model — and, because the view
// shares the concurrency limit, what makes the owner's Close cover in-flight
// queries. Its Close is a no-op, so the caller may defer closeEmbedder on both.
func queryEmbedderFor(cfg *config.Config, document embedder.Embedder) (embedder.Embedder, error) {
	if local, ok := document.(*localembed.Embedder); ok {
		return local.WithPrefix(local.QueryPrefix()), nil
	}
	return newEmbedder(cfg, roleQuery)
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
