package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/urfave/cli/v3"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/query"
)

// graphFlags are shared by the graph traversal commands.
func graphFlags(extra ...cli.Flag) []cli.Flag {
	return slices.Concat(commonFlags, []cli.Flag{
		&cli.IntFlag{
			Name:  "limit",
			Usage: "maximum number of results to print",
			Value: 50,
		},
	}, extra)
}

func callersCmd() *cli.Command {
	return &cli.Command{
		Name:      "callers",
		Usage:     "list the symbols that reference a symbol",
		ArgsUsage: "<symbol>",
		Flags:     graphFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runGraphQuery(ctx, cmd, func(q *query.Querier, symbol string) ([]query.GraphRef, error) {
				return q.Callers(ctx, symbol)
			}, query.RefFormat{Relation: "callers of", PreferRefSite: true})
		},
	}
}

func calleesCmd() *cli.Command {
	return &cli.Command{
		Name:      "callees",
		Usage:     "list what a symbol references",
		ArgsUsage: "<symbol>",
		Flags:     graphFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runGraphQuery(ctx, cmd, func(q *query.Querier, symbol string) ([]query.GraphRef, error) {
				return q.Callees(ctx, symbol)
			}, query.RefFormat{Relation: "referenced by"})
		},
	}
}

func impactCmd() *cli.Command {
	return &cli.Command{
		Name:      "impact",
		Usage:     "list the symbols transitively affected by changing a symbol",
		ArgsUsage: "<symbol>",
		Flags: graphFlags(&cli.IntFlag{
			Name:  "depth",
			Usage: "how many levels of callers to traverse",
			Value: 2,
		}),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			depth := cmd.Int("depth")
			return runGraphQuery(ctx, cmd, func(q *query.Querier, symbol string) ([]query.GraphRef, error) {
				return q.Impact(ctx, symbol, depth)
			}, query.RefFormat{Relation: "affected by changing", ShowDepth: true, PreferRefSite: true})
		},
	}
}

// runGraphQuery wires up the store and querier, runs the given traversal, and
// prints the refs using the same renderer the MCP tools use. Graph queries need
// no embedding call, but an embedder is still constructed because the store
// validates its model and dimensions.
func runGraphQuery(
	ctx context.Context,
	cmd *cli.Command,
	run func(*query.Querier, string) ([]query.GraphRef, error),
	format query.RefFormat,
) error {
	symbol := cmd.Args().First()
	if symbol == "" {
		return errors.New("a symbol name is required")
	}

	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	if !cfg.GraphEnabled() {
		return errors.New("code graph is disabled; remove enable_graph: false from settings to use this command")
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

	refs, err := run(query.New(emb, s), symbol)
	if err != nil {
		if errors.Is(err, query.ErrGraphNotBuilt) {
			fmt.Println("No code graph in this index yet. Run 'codamigo index' to build it.")
			return nil
		}
		return err
	}

	format.Limit = cmd.Int("limit")
	fmt.Print(query.FormatRefs(refs, symbol, format))
	return nil
}
