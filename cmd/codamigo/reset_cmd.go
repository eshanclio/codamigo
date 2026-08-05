package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/ieshan/codamigo/config"
	"github.com/urfave/cli/v3"
)

func resetCmd() *cli.Command {
	return &cli.Command{
		Name:  "reset",
		Usage: "delete the vector store database",
		Flags: slices.Concat(commonFlags, []cli.Flag{
			&cli.BoolFlag{
				Name:  "force",
				Usage: "skip the confirmation prompt",
			},
		}),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			storePath, err := config.DefaultStorePath(cfg.ProjectRoot)
			if err != nil {
				return fmt.Errorf("resolving store path: %w", err)
			}
			return runReset(storePath, cmd.Bool("force"), os.Stdin, os.Stdout)
		},
	}
}

// runReset deletes the store file at storePath. It prompts for confirmation
// unless force is true. in and out are used for the prompt so the function
// is testable without touching os.Stdin/os.Stdout.
func runReset(storePath string, force bool, in io.Reader, out io.Writer) error {
	if _, err := os.Stat(storePath); errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintln(out, "Nothing to reset.")
		return nil
	}

	if !force {
		_, _ = fmt.Fprintf(out, "The following file will be deleted:\n  %s\nDelete store file? [y/N]: ", storePath)
		scanner := bufio.NewScanner(in)
		answer := ""
		if scanner.Scan() {
			answer = strings.TrimSpace(scanner.Text())
		}
		if !strings.EqualFold(answer, "y") {
			_, _ = fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(storePath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("deleting store%s: %w", suffix, err)
		}
	}
	_, _ = fmt.Fprintf(out, "Store deleted: %s\n", storePath)
	return nil
}
