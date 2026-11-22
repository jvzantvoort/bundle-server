package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/jvzantvoort/bundle-server/internal/config"
	"github.com/jvzantvoort/bundle-server/internal/server"
	"github.com/jvzantvoort/bundle-server/internal/taskqueue"
	"github.com/spf13/cobra"
)

// version and revision are set at build time with -ldflags.
var version string
var revision string

// versionString returns the version set at build time. Binaries from go install
// have no build flags, so it falls back to the module version.
func versionString() string {
	current := version
	if current == "" {
		current = "dev"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			current = info.Main.Version
		}
	}
	if revision != "" {
		current += " (" + revision + ")"
	}
	return current
}

func main() {
	root := &cobra.Command{
		Use:     "bundle-server",
		Short:   "Serve and manage Bundle pools",
		Version: versionString(),
	}
	root.AddCommand(newServeCommand(), newConfigCommand())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func newServeCommand() *cobra.Command {
	var configPath, specPath string
	command := &cobra.Command{
		Use:   "serve",
		Short: "Start the Bundle HTTP server",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			spec, err := os.ReadFile(specPath)
			if err != nil {
				return fmt.Errorf("read OpenAPI specification: %w", err)
			}
			queue := taskqueue.NewMemoryQueue(cfg.Server.TaskWorkers, nil)
			defer queue.Close()
			app, err := server.New(cfg, queue, spec)
			if err != nil {
				return err
			}
			app.SetVersion(versionString())
			schedules, err := app.StartSchedules(context.Background())
			if err != nil {
				return fmt.Errorf("start schedules: %w", err)
			}
			defer schedules.Stop()
			httpServer := &http.Server{
				Addr:              cfg.Server.Address,
				Handler:           app.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       60 * time.Second,
			}
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-stop
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := httpServer.Shutdown(ctx); err != nil {
					log.Printf("shutdown failed: %v", err)
				}
			}()
			log.Printf("Bundle server %s listening on %s", versionString(), cfg.Server.Address)
			err = httpServer.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		},
	}
	command.Flags().StringVarP(&configPath, "config", "c", "config.yaml", "server configuration file")
	command.Flags().StringVar(&specPath, "openapi", "openapi.yaml", "OpenAPI specification file")
	return command
}

func newConfigCommand() *cobra.Command {
	var configPath string
	command := &cobra.Command{
		Use:   "config",
		Short: "Manage server configuration",
	}
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate a server configuration file",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			fmt.Printf("Configuration valid: %d pool(s), listening on %s\n", len(cfg.Pools), cfg.Server.Address)
			return nil
		},
	}
	validate.Flags().StringVarP(&configPath, "config", "c", "config.yaml", "server configuration file")
	command.AddCommand(validate)
	return command
}
