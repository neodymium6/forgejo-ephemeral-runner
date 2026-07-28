package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/neodymium6/forgejo-ephemeral-runner/internal/controller"
)

func main() {
	logger := log.New(os.Stdout, "controller: ", log.LstdFlags|log.LUTC)

	cfg, err := controller.ConfigFromEnvironment()
	if err != nil {
		logger.Printf("invalid configuration: %v", err)
		os.Exit(2)
	}

	forgejoClient, err := controller.NewForgejoClient(cfg)
	if err != nil {
		logger.Printf("create Forgejo client: %v", err)
		os.Exit(2)
	}
	kubernetesClient, err := controller.NewKubernetesClient(cfg)
	if err != nil {
		logger.Printf("create Kubernetes client: %v", err)
		os.Exit(2)
	}
	leaderElector, err := controller.NewLeaderElector(cfg, kubernetesClient, logger)
	if err != nil {
		logger.Printf("create leader elector: %v", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = leaderElector.Run(ctx, func(leaderCtx context.Context) error {
		return controller.Run(leaderCtx, cfg, forgejoClient, kubernetesClient, logger)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}

	logger.Print("stopped")
}
