package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/neodymium6/forgejo-ephemeral-runner/internal/reaper"
)

func main() {
	logger := log.New(os.Stdout, "reaper: ", log.LstdFlags|log.LUTC)

	cfg, err := reaper.ConfigFromEnvironment()
	if err != nil {
		logger.Printf("invalid configuration: %v", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := reaper.NewClient(cfg)
	if err != nil {
		logger.Printf("create Kubernetes client: %v", err)
		os.Exit(2)
	}

	if err := reaper.Run(ctx, cfg, client, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stdout, "reaper: stopped")
}
