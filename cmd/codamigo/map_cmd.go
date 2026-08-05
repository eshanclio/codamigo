package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/urfave/cli/v3"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/query"
)

func mapCmd() *cli.Command {
	return &cli.Command{
		Name:  "map",
		Usage: "print a structural map of the indexed codebase",
		Flags: slices.Concat(commonFlags, []cli.Flag{
			&cli.IntFlag{
				Name:  "max-tokens",
				Usage: "token budget for the map output",
				Value: 2000,
			},
			&cli.BoolFlag{
				Name:  "no-code-only",
				Usage: "include configured non-code language files in the map",
			},
			&cli.BoolFlag{
				Name:  "no-summary",
				Usage: "hide per-file type summary from file headers",
			},
			&cli.BoolFlag{
				Name:  "no-visibility",
				Usage: "hide export/visibility markers from symbols",
			},
		}),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			emb, err := newEmbedder(cfg, roleQuery)
			if err != nil {
				return fmt.Errorf("creating embedder: %w", err)
			}
			defer closeEmbedder(emb)
			storePath, err := config.DefaultStorePath(cfg.ProjectRoot)
			if err != nil {
				return fmt.Errorf("resolving store path: %w", err)
			}
			s, err := buildStore(storePath, cfg.EmbeddingModel, emb.Dim())
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }() // best-effort cleanup; the process is exiting either way

			q := query.New(emb, s)

			maxTokens := cmd.Int("max-tokens")
			if maxTokens < 0 {
				maxTokens = 0
			}
			opts := query.MapOptions{
				MaxTokens:        maxTokens,
				CodeOnly:         !cmd.Bool("no-code-only"),
				NonCodeLanguages: cfg.NonCodeLanguages,
				ShowSummary:      !cmd.Bool("no-summary"),
				ShowVisibility:   !cmd.Bool("no-visibility"),
			}
			result, err := q.Map(ctx, opts)
			if err != nil {
				return err
			}
			if result == "" {
				fmt.Println("No symbols indexed yet. Run 'codamigo index' first.")
				return nil
			}
			fmt.Print(result)
			return nil
		},
	}
}
