package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/OmniLLM/omni-agent-hub/internal/config"
)

func newConfigInitCmd(_ *Opts) *cobra.Command {
	var (
		path     string
		defaults bool
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new config file with an interactive wizard",
		Long: `Create a new config.yaml interactively.

Prompts for each setting with sensible defaults. Use --defaults to
write the default config without prompting (keys are auto-generated).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				path = config.DefaultConfigPath()
			}
			out := cmd.OutOrStdout()

			// Check for existing file.
			if _, err := os.Stat(path); err == nil {
				if !force {
					if !confirm(fmt.Sprintf("Config file %s already exists. Overwrite?", path), false) {
						fmt.Fprintln(out, "Cancelled.")
						return nil
					}
				}
			}

			cfg := config.DefaultConfig()

			if defaults {
				// Non-interactive: just generate keys and write.
				cfg.Server.APIKey = generateHexKey()
				cfg.Server.AdminKey = generateHexKey()
			} else {
				// Interactive wizard.
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, bold("── Omni A2A Hub Configuration Wizard ──"))
				fmt.Fprintln(out, "Press Enter to accept the default value shown in [brackets].")
				fmt.Fprintln(out, "")

				// Server settings.
				fmt.Fprintln(out, bold("Server"))
				cfg.Server.Host = readLineDefault("  Host", cfg.Server.Host)
				cfg.Server.Port = readIntDefault("  Port", cfg.Server.Port)
				cfg.Server.PublicURL = readLineDefault("  Public URL", cfg.Server.PublicURL)

				// API keys.
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, bold("Authentication"))
				apiKeyChoice := readChoice("  API key:", []string{
					"Auto-generate (recommended)",
					"Enter manually",
				})
				if apiKeyChoice == 1 {
					cfg.Server.APIKey = readRequiredLine("  API key: ")
				} else {
					cfg.Server.APIKey = generateHexKey()
					fmt.Fprintf(out, "  Generated API key: %s…\n", cfg.Server.APIKey[:16])
				}

				adminKeyChoice := readChoice("  Admin key:", []string{
					"Auto-generate (recommended)",
					"Enter manually",
				})
				if adminKeyChoice == 1 {
					cfg.Server.AdminKey = readRequiredLine("  Admin key: ")
				} else {
					cfg.Server.AdminKey = generateHexKey()
					fmt.Fprintf(out, "  Generated Admin key: %s…\n", cfg.Server.AdminKey[:16])
				}

				// Hub identity.
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, bold("Hub Identity"))
				cfg.Hub.Name = readLineDefault("  Hub name", cfg.Hub.Name)
				cfg.Hub.Description = readLineDefault("  Description", cfg.Hub.Description)

				// Storage.
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, bold("Storage"))
				cfg.Storage.Path = readLineDefault("  Database path", cfg.Storage.Path)
				cfg.Storage.AuditRetention = readIntDefault("  Audit retention (rows)", cfg.Storage.AuditRetention)

				// Logging.
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, bold("Logging"))
				cfg.Logging.File = readLineDefault("  Log file", cfg.Logging.File)
				logLevelIdx := readChoice("  Log level:", []string{"info", "debug", "warn", "error"})
				if logLevelIdx >= 0 {
					cfg.Logging.Level = []string{"info", "debug", "warn", "error"}[logLevelIdx]
				}
				logFormatIdx := readChoice("  Log format:", []string{"json", "text"})
				if logFormatIdx >= 0 {
					cfg.Logging.Format = []string{"json", "text"}[logFormatIdx]
				}

				// Optional: add upstream.
				fmt.Fprintln(out, "")
				if confirm("Add an upstream agent now?", false) {
					for {
						up := promptUpstream()
						if up != nil {
							cfg.Upstream = append(cfg.Upstream, *up)
						}
						if !confirm("Add another upstream?", false) {
							break
						}
					}
				}

				// Show summary.
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, bold("── Summary ──"))
				fmt.Fprintf(out, "  Server         : %s:%d\n", cfg.Server.Host, cfg.Server.Port)
				fmt.Fprintf(out, "  Public URL     : %s\n", cfg.Server.PublicURL)
				fmt.Fprintf(out, "  Hub name       : %s\n", cfg.Hub.Name)
				fmt.Fprintf(out, "  Storage        : %s\n", cfg.Storage.Path)
				fmt.Fprintf(out, "  Log file       : %s\n", cfg.Logging.File)
				fmt.Fprintf(out, "  Log level      : %s\n", cfg.Logging.Level)
				fmt.Fprintf(out, "  Upstreams      : %d\n", len(cfg.Upstream))
				fmt.Fprintf(out, "  Config path    : %s\n", path)
				fmt.Fprintln(out, "")

				if !confirm("Write this configuration?", true) {
					fmt.Fprintln(out, "Cancelled.")
					return nil
				}
			}

			// Ensure parent directory exists.
			if dir := filepath.Dir(path); dir != "" {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("creating config dir: %w", err)
				}
			}

			if err := config.Save(cfg, path); err != nil {
				return fmt.Errorf("writing config: %w", err)
			}
			fmt.Fprintf(out, "✓ Config written to %s\n", path)
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "config file path (default: ~/.config/omni-agent-hub/config.yaml)")
	cmd.Flags().BoolVar(&defaults, "defaults", false, "write defaults with auto-generated keys (non-interactive)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing config without confirmation")
	return cmd
}

// promptUpstream interactively collects upstream configuration.
func promptUpstream() *config.UpstreamCfg {
	name := readRequiredLine("  Upstream name: ")
	baseURL := readRequiredLine("  Base URL: ")
	prefix := readLineDefault("  Prefix", "@"+name)

	idx := readChoice("  Auth scheme:", []string{"bearer", "none"})
	scheme := "none"
	if idx >= 0 {
		scheme = []string{"bearer", "none"}[idx]
	}
	token := ""
	if scheme == "bearer" {
		token = readLine("  Bearer token: ")
	}

	return &config.UpstreamCfg{
		Name:    name,
		BaseURL: baseURL,
		Prefix:  prefix,
		Auth: config.AuthConfig{
			Scheme: scheme,
			Token:  token,
		},
		Enabled: true,
	}
}

// generateHexKey generates a 32-byte hex-encoded random key.
func generateHexKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "CHANGE_ME_key_generation_failed"
	}
	return hex.EncodeToString(b)
}
