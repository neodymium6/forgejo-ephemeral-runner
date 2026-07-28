package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

const managedDescription = "Managed by forgejo-ephemeral-runner"

type Registration struct {
	ID    int64  `json:"id"`
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

type RemoteRunner struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Ephemeral   bool   `json:"ephemeral"`
}

type RemoteJob struct {
	ID      int64    `json:"id"`
	Attempt int64    `json:"attempt"`
	Handle  string   `json:"handle"`
	RunsOn  []string `json:"runs_on"`
	Status  string   `json:"status"`
}

type PodState struct {
	Exists   bool
	Deleting bool
	Phase    string
}

type CredentialState struct {
	Exists   bool
	RunnerID int64
}

type Forgejo interface {
	ListJobs(context.Context, []string) ([]RemoteJob, error)
	ListRunners(context.Context) ([]RemoteRunner, error)
	RegisterRunner(context.Context, string, string) (Registration, error)
	DeleteRunner(context.Context, int64) error
}

type Kubernetes interface {
	GetPod(context.Context) (PodState, error)
	CreatePod(context.Context) error
	DeletePod(context.Context) error
	GetCredential(context.Context) (CredentialState, error)
	CreateCredential(context.Context, Registration) error
	DeleteCredential(context.Context) error
}

func Run(
	ctx context.Context,
	cfg Config,
	forgejo Forgejo,
	kubernetes Kubernetes,
	logger *log.Logger,
) error {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	logger.Printf("maintaining one ephemeral runner %q in namespace %q", cfg.RunnerName, cfg.Namespace)
	for {
		if err := Reconcile(ctx, cfg, forgejo, kubernetes, logger); err != nil {
			logger.Printf("reconcile failed; will retry: %v", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func Reconcile(
	ctx context.Context,
	cfg Config,
	forgejo Forgejo,
	kubernetes Kubernetes,
	logger *log.Logger,
) error {
	pod, err := kubernetes.GetPod(ctx)
	if err != nil {
		return fmt.Errorf("get runner pod: %w", err)
	}
	credential, err := kubernetes.GetCredential(ctx)
	if err != nil {
		return fmt.Errorf("get runner credential: %w", err)
	}

	if pod.Exists {
		if !credential.Exists {
			logger.Printf("deleting orphan runner pod %q", cfg.RunnerPodName)
			return kubernetes.DeletePod(ctx)
		}
		if pod.Deleting {
			return nil
		}
		switch pod.Phase {
		case "Succeeded", "Failed":
			logger.Printf("deleting terminal runner pod %q with phase %s", cfg.RunnerPodName, pod.Phase)
			return kubernetes.DeletePod(ctx)
		default:
			return nil
		}
	}

	if credential.Exists {
		if credential.RunnerID > 0 {
			logger.Printf("removing Forgejo runner registration %d", credential.RunnerID)
			if err := forgejo.DeleteRunner(ctx, credential.RunnerID); err != nil {
				return fmt.Errorf("delete Forgejo runner %d: %w", credential.RunnerID, err)
			}
		}
		logger.Printf("deleting consumed runner credential %q", cfg.CredentialSecretName)
		return kubernetes.DeleteCredential(ctx)
	}

	stale, err := forgejo.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("list Forgejo runners: %w", err)
	}
	removedStale := false
	for _, runner := range stale {
		if runner.Name != cfg.RunnerName {
			continue
		}
		if runner.Description != managedDescription || !runner.Ephemeral {
			return fmt.Errorf(
				"runner name %q is already used by an unmanaged or non-ephemeral runner",
				cfg.RunnerName,
			)
		}
		if runner.ID <= 0 {
			return fmt.Errorf("managed runner %q has an invalid ID", cfg.RunnerName)
		}
		logger.Printf("removing stale Forgejo runner registration %d", runner.ID)
		if err := forgejo.DeleteRunner(ctx, runner.ID); err != nil {
			return fmt.Errorf("delete stale Forgejo runner %d: %w", runner.ID, err)
		}
		removedStale = true
	}
	if removedStale {
		return nil
	}

	registration, err := forgejo.RegisterRunner(ctx, cfg.RunnerName, managedDescription)
	if err != nil {
		return fmt.Errorf("register ephemeral Forgejo runner: %w", err)
	}
	logger.Printf("created ephemeral Forgejo runner registration %d", registration.ID)

	if err := kubernetes.CreateCredential(ctx, registration); err != nil {
		cleanupErr := forgejo.DeleteRunner(ctx, registration.ID)
		return errors.Join(fmt.Errorf("create runner credential: %w", err), cleanupErr)
	}
	if err := kubernetes.CreatePod(ctx); err != nil {
		credentialErr := kubernetes.DeleteCredential(ctx)
		registrationErr := forgejo.DeleteRunner(ctx, registration.ID)
		return errors.Join(fmt.Errorf("create runner pod: %w", err), credentialErr, registrationErr)
	}
	logger.Printf("created runner pod %q", cfg.RunnerPodName)
	return nil
}
