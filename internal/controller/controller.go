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
	Exists    bool
	RunnerID  int64
	JobHandle string
}

type Forgejo interface {
	ListJobs(context.Context, []string) ([]RemoteJob, error)
	ListRunners(context.Context) ([]RemoteRunner, error)
	RegisterRunner(context.Context, string, string) (Registration, error)
	DeleteRunner(context.Context, int64) error
}

type Kubernetes interface {
	GetPod(context.Context, int) (PodState, error)
	CreatePod(context.Context, int) error
	DeletePod(context.Context, int) error
	GetCredential(context.Context, int) (CredentialState, error)
	CreateCredential(context.Context, int, Registration, string) error
	DeleteCredential(context.Context, int) error
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

	logger.Printf(
		"scaling ephemeral runners %q to queued jobs in namespace %q (maximum %d)",
		cfg.RunnerName,
		cfg.Namespace,
		cfg.MaxConcurrent,
	)
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
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > maxSupportedConcurrent {
		return fmt.Errorf("MaxConcurrent must be between 1 and %d", maxSupportedConcurrent)
	}

	type localSlot struct {
		pod        PodState
		credential CredentialState
	}
	slots := make([]localSlot, cfg.MaxConcurrent)
	activeHandles := make(map[string]struct{}, cfg.MaxConcurrent)
	referencedRunners := make(map[int64]struct{}, cfg.MaxConcurrent)
	activeCount := 0

	for slot := range cfg.MaxConcurrent {
		pod, err := kubernetes.GetPod(ctx, slot)
		if err != nil {
			return fmt.Errorf("get runner pod for slot %d: %w", slot, err)
		}
		credential, err := kubernetes.GetCredential(ctx, slot)
		if err != nil {
			return fmt.Errorf("get runner credential for slot %d: %w", slot, err)
		}
		slots[slot] = localSlot{pod: pod, credential: credential}

		if pod.Exists {
			if !credential.Exists {
				logger.Printf("deleting orphan runner pod in slot %d", slot)
				return kubernetes.DeletePod(ctx, slot)
			}
			if pod.Deleting {
				activeCount++
				activeHandles[credential.JobHandle] = struct{}{}
				referencedRunners[credential.RunnerID] = struct{}{}
				continue
			}
			switch pod.Phase {
			case "Succeeded", "Failed":
				logger.Printf("deleting terminal runner pod in slot %d with phase %s", slot, pod.Phase)
				return kubernetes.DeletePod(ctx, slot)
			default:
				activeCount++
				activeHandles[credential.JobHandle] = struct{}{}
				referencedRunners[credential.RunnerID] = struct{}{}
			}
			continue
		}

		if credential.Exists {
			logger.Printf("removing Forgejo runner registration %d from slot %d", credential.RunnerID, slot)
			if err := forgejo.DeleteRunner(ctx, credential.RunnerID); err != nil {
				return fmt.Errorf("delete Forgejo runner %d: %w", credential.RunnerID, err)
			}
			logger.Printf("deleting consumed runner credential in slot %d", slot)
			return kubernetes.DeleteCredential(ctx, slot)
		}
	}

	remoteRunners, err := forgejo.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("list Forgejo runners: %w", err)
	}
	remoteByName := make(map[string][]RemoteRunner)
	managedNames := make(map[string]struct{}, cfg.MaxConcurrent)
	for slot := range cfg.MaxConcurrent {
		managedNames[runnerNameForSlot(cfg, slot)] = struct{}{}
	}
	for _, runner := range remoteRunners {
		if _, managedName := managedNames[runner.Name]; !managedName {
			continue
		}
		remoteByName[runner.Name] = append(remoteByName[runner.Name], runner)
		if _, referenced := referencedRunners[runner.ID]; referenced {
			continue
		}
		if runner.Description != managedDescription || !runner.Ephemeral {
			continue
		}
		if runner.ID <= 0 {
			return fmt.Errorf("managed runner %q has an invalid ID", runner.Name)
		}
		logger.Printf("removing stale Forgejo runner registration %d", runner.ID)
		if err := forgejo.DeleteRunner(ctx, runner.ID); err != nil {
			return fmt.Errorf("delete stale Forgejo runner %d: %w", runner.ID, err)
		}
		return nil
	}

	if activeCount >= cfg.MaxConcurrent {
		return nil
	}

	jobs, err := forgejo.ListJobs(ctx, cfg.RunnerLabels)
	if err != nil {
		return fmt.Errorf("list Forgejo jobs: %w", err)
	}
	var waiting []RemoteJob
	for _, job := range jobs {
		if job.Status != "waiting" {
			continue
		}
		if job.ID <= 0 || job.Attempt < 0 || job.Handle == "" {
			return errors.New("Forgejo returned an incomplete waiting job")
		}
		if _, active := activeHandles[job.Handle]; active {
			continue
		}
		activeHandles[job.Handle] = struct{}{}
		waiting = append(waiting, job)
	}

	jobIndex := 0
	for slot, local := range slots {
		if activeCount >= cfg.MaxConcurrent || jobIndex >= len(waiting) {
			break
		}
		if local.pod.Exists || local.credential.Exists {
			continue
		}

		name := runnerNameForSlot(cfg, slot)
		for _, runner := range remoteByName[name] {
			if _, referenced := referencedRunners[runner.ID]; referenced {
				continue
			}
			return fmt.Errorf("runner name %q is already used by an unmanaged or non-ephemeral runner", name)
		}

		job := waiting[jobIndex]
		jobIndex++
		registration, err := forgejo.RegisterRunner(ctx, name, managedDescription)
		if err != nil {
			return fmt.Errorf("register ephemeral Forgejo runner for slot %d: %w", slot, err)
		}
		logger.Printf("created ephemeral Forgejo runner registration %d for slot %d", registration.ID, slot)

		if err := kubernetes.CreateCredential(ctx, slot, registration, job.Handle); err != nil {
			cleanupErr := forgejo.DeleteRunner(ctx, registration.ID)
			return errors.Join(fmt.Errorf("create runner credential for slot %d: %w", slot, err), cleanupErr)
		}
		if err := kubernetes.CreatePod(ctx, slot); err != nil {
			credentialErr := kubernetes.DeleteCredential(ctx, slot)
			registrationErr := forgejo.DeleteRunner(ctx, registration.ID)
			return errors.Join(fmt.Errorf("create runner pod for slot %d: %w", slot, err), credentialErr, registrationErr)
		}
		logger.Printf("created runner pod in slot %d for Forgejo job %d attempt %d", slot, job.ID, job.Attempt)
		activeCount++
	}
	return nil
}

func runnerNameForSlot(cfg Config, slot int) string {
	return fmt.Sprintf("%s-%d", cfg.RunnerName, slot)
}
