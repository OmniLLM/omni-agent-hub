package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/OmniLLM/omni-agent-hub/internal/card"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
	"github.com/OmniLLM/omni-agent-hub/internal/logging"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
	"github.com/OmniLLM/omni-agent-hub/internal/transport"
)

// version is set at build time via -ldflags. Falls back to "dev" for `go run`.
var version = "dev"

func newServeCmd(opts *Opts) *cobra.Command {
	var (
		host string
		port int
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Omni A2A hub in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd, opts, host, port)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "bind host (overrides config)")
	cmd.Flags().IntVar(&port, "port", 0, "bind port (overrides config)")
	return cmd
}

func runServe(cmd *cobra.Command, opts *Opts, host string, port int) error {
	cfg := config.LoadOrDefault(opts.ConfigPath)
	if host != "" {
		cfg.Server.Host = host
	}
	if port != 0 {
		cfg.Server.Port = port
	}

	// Logging first, so any following config validation warnings go through slog.
	logPath := config.ResolveLogFile(opts.LogFile, cfg)
	closer, err := logging.Setup(logPath, cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return fmt.Errorf("setting up logging to %s: %w", logPath, err)
	}
	defer closer.Close()

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config invalid: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Storage.
	dbPath := config.ExpandPath(cfg.Storage.Path)
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store %s: %w", dbPath, err)
	}
	defer db.Close()
	if err := db.VacuumAudit(ctx, cfg.Storage.AuditRetention); err != nil {
		slog.Warn("audit vacuum failed", "err", err)
	}

	// Registry + bootstrap from config.
	reg := registry.New(db, nil)
	if err := registry.Bootstrap(ctx, reg, db, cfg.Upstream); err != nil {
		return fmt.Errorf("bootstrap registry: %w", err)
	}
	// Best-effort initial card refresh. Use a detached context so the refresh
	// completes even if SIGINT arrives during startup.
	go func() {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if err := reg.RefreshAll(refreshCtx); err != nil {
			slog.Warn("initial card refresh failed", "err", err)
		}
	}()

	// Periodic background refresh so upstream cards stay current while the
	// server runs. Disabled when the interval is 0 (hub.refresh_interval: "0").
	if interval := cfg.Hub.RefreshIntervalOrDefault(); interval > 0 {
		slog.Info("periodic upstream refresh enabled", "interval", interval.String())
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
					if err := reg.RefreshAll(refreshCtx); err != nil {
						slog.Warn("periodic card refresh failed", "err", err)
					}
					cancel()
				}
			}
		}()
	}

	// Composite card builder + dispatch + transport.
	cb := card.Start(ctx, reg, card.FromConfig(cfg, version))
	disp := dispatch.New(reg, db)
	tsrv := transport.New(transport.Deps{
		Cfg: cfg, Reg: reg, Card: cb, Store: db,
		Unary: disp, Stream: disp, Version: version,
	})

	// Startup banner (once, via fmt so users see it if not tailing logs).
	bnr := cmd.OutOrStdout()
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	fmt.Fprintln(bnr)
	fmt.Fprintf(bnr, "  %s %s\n", bold(cyan("◆ Omni A2A Hub")), dim("("+version+")"))
	fmt.Fprintf(bnr, "  %s\n", dim(separator(42)))
	fmt.Fprintf(bnr, "  %s  %s\n", dim("listening"), bold("http://"+addr))
	fmt.Fprintf(bnr, "  %s  %d\n", dim("upstreams"), len(cfg.Upstream))
	fmt.Fprintf(bnr, "  %s  %s\n", dim("logs     "), logPath)
	fmt.Fprintln(bnr)
	slog.Info("hub starting",
		"host", cfg.Server.Host, "port", cfg.Server.Port,
		"upstreams", len(cfg.Upstream), "log", logPath, "db", dbPath)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           tsrv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout: SSE streams must not be capped.
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server error: %w", err)
	}
	slog.Info("hub stopped")
	return nil
}
