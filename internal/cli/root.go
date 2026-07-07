// Package cli defines the Cobra command tree for the omni-agent-hub binary.
package cli

import (
	"github.com/spf13/cobra"
)

// Opts holds the persistent (global) flags shared across subcommands.
type Opts struct {
	ConfigPath string
	LogFile    string
}

// NewRootCmd builds and returns the root Cobra command with all subcommands.
func NewRootCmd() *cobra.Command {
	opts := &Opts{}

	var (
		host string
		port int
	)

	root := &cobra.Command{
		Use:           "oah",
		Short:         "Omni A2A Hub",
		Long:          "oah (Omni A2A Hub) — aggregates multiple upstream A2A agents behind one endpoint.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Default: no subcommand → run the hub in the foreground.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd, opts, host, port)
		},
	}

	root.PersistentFlags().StringVar(&opts.ConfigPath, "config", "", "path to config YAML file")
	root.PersistentFlags().StringVar(&opts.LogFile, "log-file", "", "override log file path")

	root.Flags().StringVar(&host, "host", "", "bind host (overrides config)")
	root.Flags().IntVar(&port, "port", 0, "bind port (overrides config)")

	root.AddCommand(newServeCmd(opts))
	root.AddCommand(newStartCmd(opts))
	root.AddCommand(newStopCmd(opts))
	root.AddCommand(newRestartCmd(opts))
	root.AddCommand(newStatusCmd(opts))
	root.AddCommand(newLogsCmd(opts))
	root.AddCommand(newUpstreamCmd(opts))
	root.AddCommand(newConfigCmd(opts))
	root.AddCommand(newVersionCmd(opts))
	root.AddCommand(newHealthCmd(opts))
	root.AddCommand(newSkillsCmd(opts))
	root.AddCommand(newTaskCmd(opts))
	root.AddCommand(newAuditCmd(opts))
	root.AddCommand(newMessageCmd(opts))

	return root
}
