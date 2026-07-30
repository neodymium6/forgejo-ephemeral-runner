package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neodymium6/forgejo-ephemeral-runner/internal/controller"
)

var version = "dev"

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
	metrics := controller.NewMetrics(version)
	leaderElector, err := controller.NewLeaderElector(cfg, kubernetesClient, logger, metrics)
	if err != nil {
		logger.Printf("create leader elector: %v", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", cfg.MetricsListenAddress)
	if err != nil {
		logger.Printf("listen for metrics on %q: %v", cfg.MetricsListenAddress, err)
		os.Exit(2)
	}
	metricsServer := &http.Server{
		Handler:           metrics.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          logger,
	}
	metricsDone := make(chan error, 1)
	go func() {
		serveErr := metricsServer.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		metricsDone <- serveErr
		if serveErr != nil {
			stop()
		}
	}()
	logger.Printf("serving Prometheus metrics on %q", cfg.MetricsListenAddress)

	leaderErr := leaderElector.Run(ctx, func(leaderCtx context.Context) error {
		return controller.Run(leaderCtx, cfg, forgejoClient, kubernetesClient, logger, metrics)
	})
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownErr := metricsServer.Shutdown(shutdownCtx)
	cancelShutdown()
	metricsErr := <-metricsDone

	if errors.Is(leaderErr, context.Canceled) {
		leaderErr = nil
	}
	err = errors.Join(leaderErr, shutdownErr, metricsErr)
	if err != nil {
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}

	logger.Print("stopped")
}
