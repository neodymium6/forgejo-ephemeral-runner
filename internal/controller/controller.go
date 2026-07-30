package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

const (
	managedDescription = "Managed by forgejo-ephemeral-runner"
	managedByLabel     = "app.kubernetes.io/managed-by"
	managedByValue     = "forgejo-ephemeral-runner"
	slotLabel          = "forgejo-ephemeral-runner.dev/slot"
)

type Registration struct {
	ID    int64  `json:"id"`
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

type RemoteRunner struct {
	Scope       string `json:"-"`
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Ephemeral   bool   `json:"ephemeral"`
}

type RemoteJob struct {
	Scope   string   `json:"-"`
	ID      int64    `json:"id"`
	Attempt int64    `json:"attempt"`
	Handle  string   `json:"handle"`
	RunsOn  []string `json:"runs_on"`
	Status  string   `json:"status"`
}

type PodState struct {
	Exists    bool
	UID       string
	CreatedAt time.Time
	Deleting  bool
	Phase     string
}

type CredentialState struct {
	Exists    bool
	UID       string
	Scope     string
	RunnerID  int64
	JobHandle string
}

type Forgejo interface {
	ListJobs(context.Context, []string) ([]RemoteJob, error)
	ListRunners(context.Context) ([]RemoteRunner, error)
	RegisterRunner(context.Context, string, string, string) (Registration, error)
	DeleteRunner(context.Context, string, int64) error
}

type Kubernetes interface {
	GetPod(context.Context, int) (PodState, error)
	CreatePod(context.Context, int) error
	DeletePod(context.Context, int, string) error
	GetCredential(context.Context, int) (CredentialState, error)
	CreateCredential(context.Context, int, Registration, string, string) (string, error)
	DeleteCredential(context.Context, int, string) error
}

type scopedRunnerID struct {
	Scope string
	ID    int64
}

type scopedRunnerName struct {
	Scope string
	Name  string
}

type scopedJobHandle struct {
	Scope  string
	Handle string
}

func Run(
	ctx context.Context,
	cfg Config,
	forgejo Forgejo,
	kubernetes Kubernetes,
	logger *log.Logger,
	metrics *Metrics,
) error {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	metrics.setRunnerSlots(0, cfg.MaxConcurrent)
	metrics.setQueue(0, false)
	logger.Printf(
		"scaling ephemeral runners %q to queued jobs in namespace %q (maximum %d)",
		cfg.RunnerName,
		cfg.Namespace,
		cfg.MaxConcurrent,
	)
	for {
		started := time.Now()
		err := reconcile(ctx, cfg, forgejo, kubernetes, logger, metrics)
		metrics.observeReconcile(time.Since(started), err)
		if err != nil {
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
	return reconcile(ctx, cfg, forgejo, kubernetes, logger, nil)
}

func reconcile(
	ctx context.Context,
	cfg Config,
	forgejo Forgejo,
	kubernetes Kubernetes,
	logger *log.Logger,
	metrics *Metrics,
) error {
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > maxSupportedConcurrent {
		return fmt.Errorf("MaxConcurrent must be between 1 and %d", maxSupportedConcurrent)
	}
	if cfg.RunnerStartupTimeout <= 0 || cfg.RunnerUnknownTimeout <= 0 {
		return errors.New("runner recovery timeouts must be positive")
	}
	now := time.Now()
	metrics.setQueue(0, false)

	type localSlot struct {
		pod        PodState
		credential CredentialState
	}
	slots := make([]localSlot, cfg.MaxConcurrent)
	activeHandles := make(map[scopedJobHandle]struct{}, cfg.MaxConcurrent)
	referencedRunners := make(map[scopedRunnerID]struct{}, cfg.MaxConcurrent)
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
		if credential.Exists && !repositoryScopeAllowed(cfg.ForgejoRepositoryAllowlist, credential.Scope) {
			return fmt.Errorf("runner credential in slot %d uses scope %q outside the current repository allowlist", slot, credential.Scope)
		}
		slots[slot] = localSlot{pod: pod, credential: credential}

		if pod.Exists {
			if !credential.Exists {
				logger.Printf("deleting orphan runner pod in slot %d", slot)
				metrics.observeCleanup(cleanupOrphanPod)
				err := kubernetes.DeletePod(ctx, slot, pod.UID)
				metrics.observeOperation(operationPodDelete, err)
				return err
			}
			if pod.Deleting {
				activeCount++
				activeHandles[scopedJobHandle{Scope: credential.Scope, Handle: credential.JobHandle}] = struct{}{}
				referencedRunners[scopedRunnerID{Scope: credential.Scope, ID: credential.RunnerID}] = struct{}{}
				continue
			}
			if timeout, expired := stalledPodTimeout(pod, cfg, now); expired {
				logger.Printf("deleting %s runner pod in slot %d after %s", pod.Phase, slot, timeout)
				reason := cleanupPendingTimeout
				if pod.Phase == "Unknown" {
					reason = cleanupUnknownTimeout
				}
				metrics.observeCleanup(reason)
				err := kubernetes.DeletePod(ctx, slot, pod.UID)
				metrics.observeOperation(operationPodDelete, err)
				return err
			}
			switch pod.Phase {
			case "Succeeded", "Failed":
				logger.Printf("deleting terminal runner pod in slot %d with phase %s", slot, pod.Phase)
				reason := cleanupPodSucceeded
				if pod.Phase == "Failed" {
					reason = cleanupPodFailed
				}
				metrics.observeCleanup(reason)
				err := kubernetes.DeletePod(ctx, slot, pod.UID)
				metrics.observeOperation(operationPodDelete, err)
				return err
			default:
				activeCount++
				activeHandles[scopedJobHandle{Scope: credential.Scope, Handle: credential.JobHandle}] = struct{}{}
				referencedRunners[scopedRunnerID{Scope: credential.Scope, ID: credential.RunnerID}] = struct{}{}
			}
			continue
		}

		if credential.Exists {
			remoteRunners, err := forgejo.ListRunners(ctx)
			if err != nil {
				return fmt.Errorf("list Forgejo runners before credential cleanup: %w", err)
			}
			present, err := validateManagedRunnerByID(remoteRunners, credential.Scope, credential.RunnerID, runnerNameForSlot(cfg, slot))
			if err != nil {
				return err
			}
			if present {
				logger.Printf("removing Forgejo runner registration %d from scope %q slot %d", credential.RunnerID, credential.Scope, slot)
				err := forgejo.DeleteRunner(ctx, credential.Scope, credential.RunnerID)
				metrics.observeOperation(operationRegistrationDelete, err)
				if err != nil {
					return fmt.Errorf("delete Forgejo runner %d from scope %q: %w", credential.RunnerID, credential.Scope, err)
				}
			}
			logger.Printf("deleting consumed runner credential in slot %d", slot)
			err = kubernetes.DeleteCredential(ctx, slot, credential.UID)
			metrics.observeOperation(operationCredentialDelete, err)
			return err
		}
	}
	metrics.setRunnerSlots(activeCount, cfg.MaxConcurrent)

	remoteRunners, err := forgejo.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("list Forgejo runners: %w", err)
	}
	remoteByName := make(map[scopedRunnerName][]RemoteRunner)
	managedNames := make(map[string]struct{}, cfg.MaxConcurrent)
	for slot := range cfg.MaxConcurrent {
		managedNames[runnerNameForSlot(cfg, slot)] = struct{}{}
	}
	for _, runner := range remoteRunners {
		if runner.Scope == "" {
			return errors.New("Forgejo returned a runner without a scope")
		}
		if !repositoryScopeAllowed(cfg.ForgejoRepositoryAllowlist, runner.Scope) {
			return fmt.Errorf("Forgejo returned runner scope %q outside the current repository allowlist", runner.Scope)
		}
		if _, managedName := managedNames[runner.Name]; !managedName {
			continue
		}
		key := scopedRunnerName{Scope: runner.Scope, Name: runner.Name}
		remoteByName[key] = append(remoteByName[key], runner)
		if _, referenced := referencedRunners[scopedRunnerID{Scope: runner.Scope, ID: runner.ID}]; referenced {
			continue
		}
		if runner.Description != managedDescription || !runner.Ephemeral {
			continue
		}
		if runner.ID <= 0 {
			return fmt.Errorf("managed runner %q has an invalid ID", runner.Name)
		}
		logger.Printf("removing stale Forgejo runner registration %d from scope %q", runner.ID, runner.Scope)
		metrics.observeCleanup(cleanupStaleRegistration)
		err := forgejo.DeleteRunner(ctx, runner.Scope, runner.ID)
		metrics.observeOperation(operationRegistrationDelete, err)
		if err != nil {
			return fmt.Errorf("delete stale Forgejo runner %d from scope %q: %w", runner.ID, runner.Scope, err)
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
		if job.Scope == "" || job.ID <= 0 || job.Attempt < 0 || job.Handle == "" {
			return errors.New("Forgejo returned an incomplete waiting job")
		}
		key := scopedJobHandle{Scope: job.Scope, Handle: job.Handle}
		if !repositoryScopeAllowed(cfg.ForgejoRepositoryAllowlist, job.Scope) {
			return fmt.Errorf("Forgejo returned job scope %q outside the current repository allowlist", job.Scope)
		}
		if _, active := activeHandles[key]; active {
			continue
		}
		activeHandles[key] = struct{}{}
		waiting = append(waiting, job)
	}
	metrics.setQueue(len(waiting), true)

	jobIndex := 0
	for slot, local := range slots {
		if activeCount >= cfg.MaxConcurrent || jobIndex >= len(waiting) {
			break
		}
		if local.pod.Exists || local.credential.Exists {
			continue
		}

		job := waiting[jobIndex]
		jobIndex++
		name := runnerNameForSlot(cfg, slot)
		nameKey := scopedRunnerName{Scope: job.Scope, Name: name}
		for _, runner := range remoteByName[nameKey] {
			if _, referenced := referencedRunners[scopedRunnerID{Scope: runner.Scope, ID: runner.ID}]; referenced {
				continue
			}
			return fmt.Errorf("runner name %q is already used in scope %q by an unmanaged or non-ephemeral runner", name, job.Scope)
		}

		registration, err := forgejo.RegisterRunner(ctx, job.Scope, name, managedDescription)
		metrics.observeOperation(operationRegistrationCreate, err)
		if err != nil {
			return fmt.Errorf("register ephemeral Forgejo runner for scope %q slot %d: %w", job.Scope, slot, err)
		}
		logger.Printf("created ephemeral Forgejo runner registration %d for scope %q slot %d", registration.ID, job.Scope, slot)

		credentialUID, err := kubernetes.CreateCredential(ctx, slot, registration, job.Scope, job.Handle)
		metrics.observeOperation(operationCredentialCreate, err)
		if err != nil {
			cleanupErr := forgejo.DeleteRunner(ctx, job.Scope, registration.ID)
			metrics.observeOperation(operationRegistrationDelete, cleanupErr)
			return errors.Join(fmt.Errorf("create runner credential for slot %d: %w", slot, err), cleanupErr)
		}
		err = kubernetes.CreatePod(ctx, slot)
		metrics.observeOperation(operationPodCreate, err)
		if err != nil {
			credentialErr := kubernetes.DeleteCredential(ctx, slot, credentialUID)
			metrics.observeOperation(operationCredentialDelete, credentialErr)
			registrationErr := forgejo.DeleteRunner(ctx, job.Scope, registration.ID)
			metrics.observeOperation(operationRegistrationDelete, registrationErr)
			return errors.Join(fmt.Errorf("create runner pod for slot %d: %w", slot, err), credentialErr, registrationErr)
		}
		logger.Printf("created runner pod in slot %d for Forgejo scope %q job %d attempt %d", slot, job.Scope, job.ID, job.Attempt)
		activeCount++
		metrics.setRunnerSlots(activeCount, cfg.MaxConcurrent)
	}
	return nil
}

func stalledPodTimeout(pod PodState, cfg Config, now time.Time) (time.Duration, bool) {
	var timeout time.Duration
	switch pod.Phase {
	case "Pending":
		timeout = cfg.RunnerStartupTimeout
	case "Unknown":
		timeout = cfg.RunnerUnknownTimeout
	default:
		return 0, false
	}
	if pod.CreatedAt.IsZero() || now.Before(pod.CreatedAt) {
		return timeout, false
	}
	return timeout, now.Sub(pod.CreatedAt) >= timeout
}

func validateManagedRunnerByID(runners []RemoteRunner, scope string, id int64, expectedName string) (bool, error) {
	for _, runner := range runners {
		if runner.Scope != scope || runner.ID != id {
			continue
		}
		if runner.Name != expectedName || runner.Description != managedDescription || !runner.Ephemeral {
			return false, fmt.Errorf("Forgejo runner %d in scope %q does not match managed slot %q", id, scope, expectedName)
		}
		return true, nil
	}
	return false, nil
}

func runnerNameForSlot(cfg Config, slot int) string {
	return fmt.Sprintf("%s-%d", cfg.RunnerName, slot)
}

func repositoryScopeAllowed(allowlist []string, scope string) bool {
	for _, allowed := range allowlist {
		if scope == allowed {
			return true
		}
	}
	return false
}
