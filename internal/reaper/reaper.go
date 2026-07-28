package reaper

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCompletionFile = "/var/run/forgejo-ephemeral-runner/completed"
	defaultContainerName  = "runner"
	defaultPollInterval   = 2 * time.Second
	defaultTokenPath      = "/var/run/secrets/forgejo-reaper/token"
	defaultCAPath         = "/var/run/secrets/forgejo-reaper/ca.crt"
)

// Config contains only pod-local identity and Kubernetes API connection data.
// It intentionally contains no Forgejo credentials.
type Config struct {
	APIURL          string
	Namespace       string
	PodName         string
	RunnerContainer string
	CompletionFile  string
	TokenPath       string
	CAPath          string
	PollInterval    time.Duration
}

type podStatus struct {
	Metadata struct {
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		ContainerStatuses []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type containerStatus struct {
	Name         string `json:"name"`
	RestartCount int32  `json:"restartCount"`
	State        struct {
		Terminated *struct{} `json:"terminated"`
	} `json:"state"`
}

// Client performs the two API operations the reaper needs on its own Pod.
type Client interface {
	GetPod(context.Context) (podStatus, error)
	DeletePod(context.Context) error
}

type kubernetesClient struct {
	httpClient *http.Client
	podURL     string
	token      string
}

func ConfigFromEnvironment() (Config, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	apiURL := strings.TrimSpace(os.Getenv("KUBERNETES_API_URL"))
	if apiURL == "" {
		if host == "" {
			return Config{}, errors.New("KUBERNETES_SERVICE_HOST is required")
		}
		if port == "" {
			port = "443"
		}
		apiURL = "https://" + net.JoinHostPort(host, port)
	}

	namespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	if namespace == "" {
		return Config{}, errors.New("POD_NAMESPACE is required")
	}
	podName := strings.TrimSpace(os.Getenv("POD_NAME"))
	if podName == "" {
		return Config{}, errors.New("POD_NAME is required")
	}

	pollInterval := defaultPollInterval
	if raw := strings.TrimSpace(os.Getenv("POLL_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse POLL_INTERVAL: %w", err)
		}
		if parsed < 250*time.Millisecond {
			return Config{}, errors.New("POLL_INTERVAL must be at least 250ms")
		}
		pollInterval = parsed
	}

	return Config{
		APIURL:          apiURL,
		Namespace:       namespace,
		PodName:         podName,
		RunnerContainer: environmentOrDefault("RUNNER_CONTAINER_NAME", defaultContainerName),
		CompletionFile:  environmentOrDefault("COMPLETION_FILE", defaultCompletionFile),
		TokenPath:       environmentOrDefault("SERVICE_ACCOUNT_TOKEN_FILE", defaultTokenPath),
		CAPath:          environmentOrDefault("SERVICE_ACCOUNT_CA_FILE", defaultCAPath),
		PollInterval:    pollInterval,
	}, nil
}

func NewClient(cfg Config) (Client, error) {
	baseURL, err := url.Parse(cfg.APIURL)
	if err != nil {
		return nil, fmt.Errorf("parse Kubernetes API URL: %w", err)
	}
	if baseURL.Scheme != "https" && baseURL.Scheme != "http" {
		return nil, fmt.Errorf("unsupported Kubernetes API URL scheme %q", baseURL.Scheme)
	}

	tokenBytes, err := os.ReadFile(cfg.TokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, errors.New("service account token is empty")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if baseURL.Scheme == "https" {
		caBytes, err := os.ReadFile(cfg.CAPath)
		if err != nil {
			return nil, fmt.Errorf("read service account CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("service account CA contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}
	}

	baseURL.Path = "/api/v1/namespaces/" +
		url.PathEscape(cfg.Namespace) +
		"/pods/" +
		url.PathEscape(cfg.PodName)

	return &kubernetesClient{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
		podURL: baseURL.String(),
		token:  token,
	}, nil
}

func Run(ctx context.Context, cfg Config, client Client, logger *log.Logger) error {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	logger.Printf(
		"watching pod %s/%s and completion file %s",
		cfg.Namespace,
		cfg.PodName,
		cfg.CompletionFile,
	)

	for {
		deleteReason, err := deletionReason(cfg, client, ctx)
		if err != nil {
			logger.Printf("observation failed; will retry: %v", err)
		} else if deleteReason != "" {
			logger.Printf("deleting own pod: %s", deleteReason)
			if err := client.DeletePod(ctx); err != nil {
				return fmt.Errorf("delete own pod: %w", err)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func deletionReason(cfg Config, client Client, ctx context.Context) (string, error) {
	if _, err := os.Stat(cfg.CompletionFile); err == nil {
		return "runner completion marker exists", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat completion marker: %w", err)
	}

	pod, err := client.GetPod(ctx)
	if err != nil {
		return "", err
	}
	if pod.Metadata.DeletionTimestamp != nil {
		return "", nil
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != cfg.RunnerContainer {
			continue
		}
		if status.RestartCount > 0 {
			return "runner container restarted " + strconv.Itoa(int(status.RestartCount)) + " time(s)", nil
		}
		if status.State.Terminated != nil {
			return "runner container terminated", nil
		}
		return "", nil
	}

	return "", nil
}

func (c *kubernetesClient) GetPod(ctx context.Context) (podStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.podURL, nil)
	if err != nil {
		return podStatus{}, err
	}
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return podStatus{}, fmt.Errorf("get pod: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return podStatus{}, responseError("get pod", resp)
	}

	var pod podStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pod); err != nil {
		return podStatus{}, fmt.Errorf("decode pod status: %w", err)
	}
	return pod, nil
}

func (c *kubernetesClient) DeletePod(ctx context.Context) error {
	body := strings.NewReader(`{"apiVersion":"v1","kind":"DeleteOptions","gracePeriodSeconds":5}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.podURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete pod: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNotFound:
		return nil
	default:
		return responseError("delete pod", resp)
	}
}

func (c *kubernetesClient) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "forgejo-ephemeral-runner-reaper")
}

func responseError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf(
		"%s returned HTTP %d: %s",
		operation,
		resp.StatusCode,
		strings.TrimSpace(string(body)),
	)
}

func environmentOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
