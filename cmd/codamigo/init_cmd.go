package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/embedder/openaicompat"
	"github.com/urfave/cli/v3"
)

func initCmd() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "guided first-time setup: write config files and verify the embedding model",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalPath, err := config.GlobalConfigPath()
			if err != nil {
				return fmt.Errorf("resolving global config path: %w", err)
			}
			defaults := config.Defaults()

			var baseURL, model, apiKey string

			_, statErr := os.Stat(globalPath)
			if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
				return fmt.Errorf("checking global config: %w", statErr)
			}
			if statErr == nil {
				// Global config already exists — load values for the smoke test.
				fmt.Printf("Global config already exists at %s.\n", globalPath)
				existing, err := config.Load(globalPath)
				if err != nil {
					return fmt.Errorf("loading global config: %w", err)
				}
				baseURL = existing.EmbeddingBaseURL
				model = existing.EmbeddingModel
				apiKey = existing.EmbeddingAPIKey
			} else {
				// Prompt user for global config values.
				scanner := bufio.NewScanner(os.Stdin)
				baseURL = readPrompt(scanner, os.Stdout, "Base URL", defaults.EmbeddingBaseURL)
				model = readPrompt(scanner, os.Stdout, "Model", defaults.EmbeddingModel)
				apiKey = readPrompt(scanner, os.Stdout, "API key (leave blank to set CODAMIGO_API_KEY env var instead)", "")

				// Write global config.
				if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
					return fmt.Errorf("creating config directory: %w", err)
				}
				type globalFile struct {
					EmbeddingBaseURL string `yaml:"embedding_base_url,omitempty"`
					EmbeddingModel   string `yaml:"embedding_model,omitempty"`
					EmbeddingAPIKey  string `yaml:"embedding_api_key,omitempty"`
				}
				gf := globalFile{EmbeddingBaseURL: baseURL, EmbeddingModel: model, EmbeddingAPIKey: apiKey}
				data, err := yaml.Marshal(gf)
				if err != nil {
					return fmt.Errorf("marshaling global config: %w", err)
				}
				if err := os.WriteFile(globalPath, data, 0o600); err != nil {
					return fmt.Errorf("writing global config: %w", err)
				}
				fmt.Printf("Created global config: %s\n", globalPath)
			}

			// Always create project config if missing — runs regardless of global config state.
			projectPath := config.ProjectConfigPath()
			if _, err := os.Stat(projectPath); errors.Is(err, fs.ErrNotExist) {
				if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
					return fmt.Errorf("creating project config directory: %w", err)
				}
				projectContent := "# codamigo project settings\n# include_patterns: []\n# exclude_patterns: []\n"
				if err := os.WriteFile(projectPath, []byte(projectContent), 0o644); err != nil {
					return fmt.Errorf("writing project config: %w", err)
				}
				fmt.Printf("Created project config: %s\n", projectPath)
			}

			// Update .gitignore.
			wd, wdErr := os.Getwd()
			if wdErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not determine working directory: %v\n", wdErr)
			} else if err := appendToGitignore(wd); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not update .gitignore: %v\n", err)
			}

			// Smoke-test the embedding model.
			fmt.Printf("\nTesting embedding model: %s\n", model)
			emb, err := openaicompat.New(openaicompat.Options{
				BaseURL: baseURL,
				APIKey:  apiKey,
				Model:   model,
			})
			if err != nil {
				fmt.Printf("[FAIL] Invalid embedder config: %v\n", err)
				return nil
			}
			if _, err := emb.Embed(ctx, "codamigo init test"); err != nil {
				fmt.Printf("[FAIL] Embedding model unreachable: %v\n", err)
				fmt.Println("Check your API key and base URL, or set CODAMIGO_API_KEY.")
			} else {
				fmt.Printf("[OK]  Embedding model reachable (model: %s)\n", model)
			}

			fmt.Println("\nRun 'codamigo index' to build the index.")
			return nil
		},
	}
}

// readPrompt writes a prompt line to w (showing defaultVal in brackets when non-empty),
// reads one line from scanner, and returns the trimmed input or defaultVal if the input
// is empty. Callers must reuse the same scanner across multiple prompts so that
// buffered input from piped stdin is not lost between calls.
func readPrompt(scanner *bufio.Scanner, w io.Writer, prompt, defaultVal string) string {
	if defaultVal != "" {
		fmt.Fprintf(w, "%s [%s]: ", prompt, defaultVal)
	} else {
		fmt.Fprintf(w, "%s: ", prompt)
	}
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			return line
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("reading prompt input", slog.Any("error", err))
	}
	return defaultVal
}

// appendToGitignore adds ".codamigo/" to <projectRoot>/.gitignore when .git exists.
// It creates .gitignore if absent and skips writing if the entry is already present.
func appendToGitignore(projectRoot string) error {
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	const entry = ".codamigo/"
	gitignorePath := filepath.Join(projectRoot, ".gitignore")

	content, err := os.ReadFile(gitignorePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading .gitignore: %w", err)
	}

	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil // already present
		}
	}

	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening .gitignore: %w", err)
	}

	var writeErr error
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		_, writeErr = fmt.Fprintln(f)
	}
	if writeErr == nil {
		_, writeErr = fmt.Fprintln(f, entry)
	}
	if closeErr := f.Close(); closeErr != nil && writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return fmt.Errorf("writing .gitignore: %w", writeErr)
	}
	return nil
}
