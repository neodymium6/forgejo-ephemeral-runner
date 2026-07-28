package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
)

type LeaderElector struct {
	httpClient      *http.Client
	leasesURL       string
	leaseURL        string
	token           string
	identity        string
	namespace       string
	leaseName       string
	leaseDuration   time.Duration
	renewDeadline   time.Duration
	retryPeriod     time.Duration
	now             func() time.Time
	logger          *log.Logger
	observedVersion string
	observedTime    time.Time
}

type kubernetesLease struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Metadata   struct {
		Name            string `json:"name,omitempty"`
		Namespace       string `json:"namespace,omitempty"`
		ResourceVersion string `json:"resourceVersion,omitempty"`
	} `json:"metadata"`
	Spec struct {
		HolderIdentity       string `json:"holderIdentity,omitempty"`
		LeaseDurationSeconds int32  `json:"leaseDurationSeconds,omitempty"`
		AcquireTime          string `json:"acquireTime,omitempty"`
		RenewTime            string `json:"renewTime,omitempty"`
		LeaseTransitions     int32  `json:"leaseTransitions,omitempty"`
	} `json:"spec"`
}

func NewLeaderElector(cfg Config, kubernetes *KubernetesClient, logger *log.Logger) (*LeaderElector, error) {
	baseURL, err := url.Parse(cfg.KubernetesAPIURL)
	if err != nil {
		return nil, fmt.Errorf("parse Kubernetes API URL for leader election: %w", err)
	}
	if baseURL.Scheme != "https" && baseURL.Scheme != "http" {
		return nil, fmt.Errorf("unsupported Kubernetes API URL scheme %q", baseURL.Scheme)
	}
	if kubernetes == nil || kubernetes.httpClient == nil {
		return nil, errors.New("Kubernetes client is required for leader election")
	}
	if logger == nil {
		return nil, errors.New("logger is required for leader election")
	}

	apiBase := *baseURL
	apiBase.Path = "/apis/coordination.k8s.io/v1/namespaces/" + url.PathEscape(cfg.Namespace) + "/leases"
	apiBase.RawQuery = ""
	apiBase.Fragment = ""
	leasesURL := apiBase.String()

	return &LeaderElector{
		httpClient:    kubernetes.httpClient,
		leasesURL:     leasesURL,
		leaseURL:      leasesURL + "/" + url.PathEscape(cfg.LeaderLeaseName),
		token:         kubernetes.token,
		identity:      cfg.ControllerIdentity,
		namespace:     cfg.Namespace,
		leaseName:     cfg.LeaderLeaseName,
		leaseDuration: defaultLeaseDuration,
		renewDeadline: defaultRenewDeadline,
		retryPeriod:   defaultRetryPeriod,
		now:           time.Now,
		logger:        logger,
	}, nil
}

func (e *LeaderElector) Run(ctx context.Context, runLeader func(context.Context) error) error {
	if runLeader == nil {
		return errors.New("leader callback is required")
	}

	for {
		for {
			acquired, err := e.tryAcquireOrRenewWithTimeout(ctx)
			if err != nil {
				e.logger.Printf("leader election attempt failed; will retry: %v", err)
			} else if acquired {
				break
			}
			if !waitForRetry(ctx, e.retryPeriod) {
				return ctx.Err()
			}
		}

		e.logger.Printf("acquired leader Lease %q as %q", e.leaseName, e.identity)
		leaderCtx, cancelLeader := context.WithCancel(ctx)
		leaderDone := make(chan error, 1)
		go func() {
			leaderDone <- runLeader(leaderCtx)
		}()

		lastRenew := e.now()
		ticker := time.NewTicker(e.retryPeriod)
		lost := false
		for !lost {
			select {
			case <-ctx.Done():
				ticker.Stop()
				cancelLeader()
				err := <-leaderDone
				if err != nil && !errors.Is(err, context.Canceled) {
					return err
				}
				return ctx.Err()
			case err := <-leaderDone:
				ticker.Stop()
				cancelLeader()
				if err == nil {
					return errors.New("leader callback stopped unexpectedly")
				}
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			case <-ticker.C:
				renewed, err := e.tryAcquireOrRenewWithTimeout(ctx)
				if err != nil {
					e.logger.Printf("leader Lease renewal failed: %v", err)
				}
				if renewed {
					lastRenew = e.now()
					continue
				}
				if e.now().Sub(lastRenew) >= e.renewDeadline {
					lost = true
				}
			}
		}

		ticker.Stop()
		e.logger.Printf("lost leader Lease %q; stopping active controller", e.leaseName)
		cancelLeader()
		err := <-leaderDone
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func waitForRetry(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (e *LeaderElector) tryAcquireOrRenewWithTimeout(ctx context.Context) (bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, e.retryPeriod)
	defer cancel()
	return e.tryAcquireOrRenew(attemptCtx)
}
func (e *LeaderElector) tryAcquireOrRenew(ctx context.Context) (bool, error) {

	now := e.now()
	lease, status, err := e.getLease(ctx)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		created := e.newLease(now)
		return e.createLease(ctx, created, now)
	}

	if strings.TrimSpace(lease.Metadata.ResourceVersion) == "" {
		return false, errors.New("leader Lease has no resourceVersion")
	}
	if lease.Metadata.ResourceVersion != e.observedVersion {
		e.observedVersion = lease.Metadata.ResourceVersion
		e.observedTime = now
	}
	if lease.Spec.HolderIdentity != "" && lease.Spec.HolderIdentity != e.identity &&
		e.observedTime.Add(e.leaseDuration).After(now) {
		return false, nil
	}

	desired := lease
	if desired.Spec.HolderIdentity != e.identity {
		desired.Spec.HolderIdentity = e.identity
		desired.Spec.AcquireTime = formatMicroTime(now)
		desired.Spec.LeaseTransitions++
	} else if desired.Spec.AcquireTime == "" {
		desired.Spec.AcquireTime = formatMicroTime(now)
	}
	desired.Spec.LeaseDurationSeconds = int32(e.leaseDuration / time.Second)
	desired.Spec.RenewTime = formatMicroTime(now)
	return e.updateLease(ctx, desired, now)
}

func (e *LeaderElector) newLease(now time.Time) kubernetesLease {
	var lease kubernetesLease
	lease.APIVersion = "coordination.k8s.io/v1"
	lease.Kind = "Lease"
	lease.Metadata.Name = e.leaseName
	lease.Metadata.Namespace = e.namespace
	lease.Spec.HolderIdentity = e.identity
	lease.Spec.LeaseDurationSeconds = int32(e.leaseDuration / time.Second)
	lease.Spec.AcquireTime = formatMicroTime(now)
	lease.Spec.RenewTime = formatMicroTime(now)
	return lease
}

func (e *LeaderElector) getLease(ctx context.Context) (kubernetesLease, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.leaseURL, nil)
	if err != nil {
		return kubernetesLease{}, 0, err
	}
	e.authorize(req)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return kubernetesLease{}, 0, fmt.Errorf("get leader Lease: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return kubernetesLease{}, resp.StatusCode, nil
	}
	if resp.StatusCode != http.StatusOK {
		return kubernetesLease{}, resp.StatusCode, kubernetesResponseError("get leader Lease", resp)
	}
	lease, err := decodeLease(resp.Body)
	if err != nil {
		return kubernetesLease{}, resp.StatusCode, err
	}
	return lease, resp.StatusCode, nil
}

func (e *LeaderElector) createLease(ctx context.Context, lease kubernetesLease, now time.Time) (bool, error) {
	body, err := json.Marshal(lease)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.leasesURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	e.authorize(req)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("create leader Lease: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return false, nil
	}
	if resp.StatusCode != http.StatusCreated {
		return false, kubernetesResponseError("create leader Lease", resp)
	}
	created, err := decodeLease(resp.Body)
	if err != nil {
		return false, err
	}
	e.observeLease(created, now)
	return true, nil
}

func (e *LeaderElector) updateLease(ctx context.Context, lease kubernetesLease, now time.Time) (bool, error) {
	body, err := json.Marshal(lease)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, e.leaseURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	e.authorize(req)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("update leader Lease: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, kubernetesResponseError("update leader Lease", resp)
	}
	updated, err := decodeLease(resp.Body)
	if err != nil {
		return false, err
	}
	e.observeLease(updated, now)
	return true, nil
}

func (e *LeaderElector) observeLease(lease kubernetesLease, now time.Time) {
	if lease.Metadata.ResourceVersion != "" {
		e.observedVersion = lease.Metadata.ResourceVersion
		e.observedTime = now
	}
}

func (e *LeaderElector) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultControllerUserAgent)
}

func decodeLease(reader io.Reader) (kubernetesLease, error) {
	var lease kubernetesLease
	if err := json.NewDecoder(io.LimitReader(reader, 1<<20)).Decode(&lease); err != nil {
		return kubernetesLease{}, fmt.Errorf("decode leader Lease: %w", err)
	}
	return lease, nil
}

func formatMicroTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
