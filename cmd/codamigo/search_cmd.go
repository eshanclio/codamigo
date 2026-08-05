package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/query"
)

func searchCmd() *cli.Command {
	return &cli.Command{
		Name:      "search",
		Usage:     "semantic search over the indexed store",
		ArgsUsage: "<query> [limit]",
		Flags: slices.Concat(commonFlags, []cli.Flag{
			&cli.StringSliceFlag{
				Name:  "lang",
				Usage: "filter by language (repeatable, e.g. --lang go --lang python)",
			},
			&cli.StringSliceFlag{
				Name:  "path",
				Usage: "filter by file path glob (repeatable, e.g. --path 'cmd/**')",
			},
			&cli.IntFlag{
				Name:  "offset",
				Usage: "number of results to skip",
				Value: 0,
			},
			&cli.IntFlag{
				Name:  "limit",
				Usage: "maximum results to return",
				Value: 10,
			},
			&cli.IntFlag{
				Name:  "max-tokens",
				Usage: "token budget for results (0 = no limit)",
				Value: 0,
			},
			&cli.StringFlag{
				Name:  "package",
				Usage: "filter results to a package (e.g. \"store\")",
			},
			&cli.StringFlag{
				Name:  "name",
				Usage: "filter by symbol name (e.g. \"Search\")",
			},
			&cli.StringSliceFlag{
				Name:  "node-kind",
				Usage: "filter by AST node kind (repeatable, e.g. --node-kind function_declaration)",
			},
			&cli.BoolFlag{
				Name:  "metadata-only",
				Usage: "return only file paths, line numbers, and symbol names (no source content)",
			},
		}),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.Args().Slice()
			if len(args) == 0 {
				return fmt.Errorf("usage: codamigo search <query> [limit]")
			}
			text := args[0]

			limit := cmd.Int("limit")
			// Backwards compat: second positional arg is limit if --limit was not set explicitly.
			if !cmd.IsSet("limit") && len(args) > 1 {
				if n, err := strconv.Atoi(args[1]); err == nil && n > 0 {
					limit = n
				}
			}
			switch {
			case limit <= 0:
				limit = 10
			case limit > 100:
				limit = 100
			}

			offset := cmd.Int("offset")
			if offset < 0 {
				offset = 0
			}
			langs := cmd.StringSlice("lang")
			paths := cmd.StringSlice("path")
			for _, p := range paths {
				if strings.Contains(p, "**") && !strings.HasSuffix(p, "/**") {
					return fmt.Errorf("unsupported glob %q: ** is only supported as trailing /**", p)
				}
			}

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

			maxTokens := cmd.Int("max-tokens")
			if maxTokens < 0 {
				maxTokens = 0
			}
			pkg := cmd.String("package")
			name := cmd.String("name")
			nodeKinds := cmd.StringSlice("node-kind")
			metadataOnly := cmd.Bool("metadata-only")

			q := query.New(emb, s)
			opts := query.SearchOptions{
				Limit:        limit,
				Offset:       offset,
				Languages:    langs,
				Paths:        paths,
				MaxTokens:    maxTokens,
				Package:      pkg,
				MetadataOnly: metadataOnly,
				NodeKinds:    nodeKinds,
			}
			if name != "" {
				opts.Names = []string{name}
			}
			sr, err := q.SearchWithOptions(ctx, text, opts)
			if err != nil {
				return err
			}
			for _, r := range sr.Results {
				if metadataOnly {
					fmt.Printf("%s:%d  %-20s %s\n", r.FilePath, r.StartLine, r.Name, r.NodeKind)
				} else {
					fmt.Printf("%.4f %s:%d-%d [%s]\n", r.Score, r.FilePath, r.StartLine, r.EndLine, r.Language)
					fmt.Println(r.Content)
					fmt.Println("---")
				}
			}
			if sr.Truncated {
				fmt.Println("(truncated to token budget)")
			}
			return nil
		},
	}
}
