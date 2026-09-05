// sybra-agentd is the thin remote execution worker. It initializes no board
// application, task/project store, workflow, automation, UI, or operator API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Automaat/sybra/internal/agentd"
)

func main() {
	configPath := flag.String("config", "", "path to the standalone agent daemon YAML config")
	check := flag.Bool("check-config", false, "validate configuration and exit")
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "sybra-agentd: -config is required")
		os.Exit(2)
	}
	cfg, err := agentd.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *check {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon, err := agentd.New(ctx, cfg, logger)
	if err == nil {
		err = daemon.Run(ctx)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("agentd.exit", "err", err)
		stop()
		os.Exit(1)
	}
	stop()
}
