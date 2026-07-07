package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/OmniLLM/omni-agent-hub/internal/config"
)

func newConfigCmd(opts *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configuration utilities",
	}
	cmd.AddCommand(newConfigMigrateCmd(opts))
	cmd.AddCommand(newConfigShowCmd(opts))
	cmd.AddCommand(newConfigInitCmd(opts))
	return cmd
}

func newConfigMigrateCmd(opts *Opts) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Rewrite config.yaml in the new hub shape (in place)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := opts.ConfigPath
			if path == "" {
				path = config.DefaultConfigPath()
			}
			cfg, err := config.Load(path)
			if err != nil {
				return fmt.Errorf("load %s: %w", path, err)
			}
			if err := config.Save(cfg, path); err != nil {
				return fmt.Errorf("save %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Migrated %s to the new hub shape.\n", path)
			return nil
		},
	}
}

func newConfigShowCmd(opts *Opts) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the resolved config with defaults applied",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.LoadOrDefault(opts.ConfigPath)
			fmt.Fprintf(cmd.OutOrStdout(),
				"server.host      : %s\nserver.port      : %d\nserver.public_url: %s\nhub.name         : %s\nstorage.path     : %s\nlogging.file     : %s\nupstreams        : %d\n",
				cfg.Server.Host, cfg.Server.Port, cfg.Server.PublicURL,
				cfg.Hub.Name, cfg.Storage.Path, cfg.Logging.File, len(cfg.Upstream))
			return nil
		},
	}
}
