package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"

	"github.com/urfave/cli/v3"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/walker"
	"github.com/ieshan/go-code-chunker/langs"
)

func doctorCmd() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "diagnose config, store, and embedding model health",
		Flags: slices.Concat(commonFlags, []cli.Flag{
			&cli.BoolFlag{
				Name:  "quick",
				Usage: "skip the live embedding smoke-test",
			},
		}),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			quick := cmd.Bool("quick")

			// ── 1. Global config ───────────────────────────────────────────────
			globalPath, globalPathErr := config.GlobalConfigPath()
			var globalErr error
			if globalPathErr != nil {
				fmt.Printf("[FAIL] Cannot resolve global config path: %v\n", globalPathErr)
			} else {
				_, globalErr = config.Load(globalPath)
			}
			switch {
			case errors.Is(globalErr, fs.ErrNotExist):
				fmt.Printf("[FAIL] Global config not found: %s — run 'codamigo init'\n", globalPath)
			case globalErr != nil:
				fmt.Printf("[FAIL] Global config parse error: %v\n", globalErr)
			default:
				fmt.Printf("[OK]  Global config: %s\n", globalPath)
			}

			// ── 2. Project config ──────────────────────────────────────────────
			// Resolve project root for home config path lookup (non-fatal in doctor).
			doctorProjectRoot := ""
			if cmd.IsSet("project-root") {
				doctorProjectRoot = cmd.String("project-root")
			}
			if doctorProjectRoot == "" {
				if wd, err := os.Getwd(); err == nil {
					doctorProjectRoot = wd
				}
			}
			if doctorProjectRoot != "" {
				if homeProjectPath, err := config.HomeProjectConfigPath(doctorProjectRoot); err == nil {
					if _, err := os.Stat(homeProjectPath); err == nil {
						fmt.Printf("[OK]  Home project config: %s\n", homeProjectPath)
					} else {
						fmt.Printf("[--]  Home project config not found (using in-project)\n")
					}
				}
			}
			projectPath := config.ProjectConfigPath()
			_, projectErr := config.Load(projectPath)
			switch {
			case errors.Is(projectErr, fs.ErrNotExist):
				fmt.Printf("[OK]  In-project config not found (using defaults)\n")
			case projectErr != nil:
				fmt.Printf("[FAIL] In-project config parse error: %v\n", projectErr)
			default:
				fmt.Printf("[OK]  In-project config: %s\n", projectPath)
			}

			// Load the merged config for subsequent checks.
			cfg, err := loadConfig(cmd)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			// ── 3. Store ───────────────────────────────────────────────────────
			storePath, err := config.DefaultStorePath(cfg.ProjectRoot)
			if err != nil {
				return fmt.Errorf("resolving store path: %w", err)
			}
			storeExists := false
			if _, err := os.Stat(storePath); err == nil {
				fmt.Printf("[OK]  Store: %s\n", storePath)
				storeExists = true
			} else {
				fmt.Printf("[FAIL] Store not found — run 'codamigo index'\n")
			}

			// ── 4. Index stats (only if store exists) ──────────────────────────
			if storeExists {
				s, err := buildStore(storePath, cfg.EmbeddingModel, cfg.EmbeddingDimensions)
				if err != nil {
					fmt.Printf("[FAIL] Store open error: %v\n", err)
				} else {
					defer s.Close()
					stats, err := s.Stats(ctx)
					if err != nil {
						fmt.Printf("[FAIL] Stats error: %v\n", err)
					} else {
						fmt.Printf("       Chunks: %6d\n", stats.ChunkCount)
						fmt.Printf("        Files: %6d\n", stats.FileCount)
						if len(stats.Languages) > 0 {
							fmt.Println("    Languages:")
							type langCount struct {
								lang  string
								count int
							}
							langs := make([]langCount, 0, len(stats.Languages))
							for l, c := range stats.Languages {
								langs = append(langs, langCount{l, c})
							}
							slices.SortFunc(langs, func(a, b langCount) int {
								return cmp.Compare(b.count, a.count) // descending
							})
							for _, lc := range langs {
								fmt.Printf("      %12s: %6d\n", lc.lang, lc.count)
							}
						}
					}
				}
			}

			// ── 5. Walker preview ──────────────────────────────────────────────
			// Uses the same filter construction as buildComponents so this count
			// matches what "codamigo index" actually processes. Keep in sync if
			// the language selection in buildComponents ever changes.
			if cfg.ProjectRoot != "" {
				filter := buildExtensionFilter(langs.AllLanguages())
				w, err := walker.New(cfg.ProjectRoot, cfg, walker.WithFileFilter(filter))
				if err != nil {
					fmt.Printf("[FAIL] Walker error: %v\n", err)
				} else {
					defer w.Close()
					count := 0
					errCount := 0
					for _, err := range w.Walk(ctx) {
						if err != nil {
							errCount++
							slog.Warn("walker error", slog.Any("error", err))
							continue
						}
						count++
					}
					fmt.Printf("Files matched by walker: %d\n", count)
					if errCount > 0 {
						fmt.Printf("[WARN] Walker encountered %d errors (see above)\n", errCount)
					}
				}
			}

			// ── 6. Embedding smoke-test ────────────────────────────────────────
			if !quick {
				emb, err := newEmbedder(cfg, cfg.EmbeddingQueryInputType)
				if err != nil {
					fmt.Printf("[FAIL] Invalid embedder config: %v\n", err)
					return nil
				}
				vec, err := emb.Embed(ctx, "codamigo doctor test")
				if err != nil {
					fmt.Printf("[FAIL] Embedding model unreachable: %v\n", err)
				} else {
					fmt.Printf("[OK]  Embedding model reachable (model: %s, dims: %d)\n", cfg.EmbeddingModel, len(vec))
				}
			}

			return nil
		},
	}
}
