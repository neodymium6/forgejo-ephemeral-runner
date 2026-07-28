package controller

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPITokenPath         = "/run/secrets/forgejo-api/api-token"
	defaultKubernetesToken      = "/var/run/secrets/forgejo-controller/token"
	defaultKubernetesCA         = "/var/run/secrets/forgejo-controller/ca.crt"
	defaultPodTemplatePath      = "/etc/forgejo-controller/runner-pod.json"
	defaultRunnerPodName        = "forgejo-ephemeral-runner-job"
	defaultCredentialName       = "forgejo-ephemeral-runner-credential"
	defaultPollInterval         = 2 * time.Second
	defaultRunnerStartupTimeout = 30 * time.Minute
	defaultRunnerUnknownTimeout = 5 * time.Minute
	defaultControllerUserAgent  = "forgejo-ephemeral-runner-controller"
	defaultMaxConcurrent        = 1
	defaultLeaderLeaseName      = "forgejo-ephemeral-runner-controller"
	maxSupportedConcurrent      = 10
)

var (
	dnsLabel       = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	imageReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
)

type Config struct {
	ForgejoURL                  string
	ForgejoScope                string
	ForgejoAPITokenPath         string
	ForgejoAllowInsecureHTTP    bool
	KubernetesAllowInsecureHTTP bool
	Namespace                   string
	RunnerName                  string
	RunnerLabels                []string
	ControllerIdentity          string
	LeaderLeaseName             string
	RunnerPodName               string
	RunnerImage                 string
	CredentialSecretName        string
	PodTemplatePath             string
	KubernetesAPIURL            string
	KubernetesTokenPath         string
	KubernetesCAPath            string
	PollInterval                time.Duration
	RunnerStartupTimeout        time.Duration
	RunnerUnknownTimeout        time.Duration
	MaxConcurrent               int
}

func ConfigFromEnvironment() (Config, error) {
	cfg := Config{
		ForgejoURL:                  strings.TrimSpace(os.Getenv("FORGEJO_INSTANCE_URL")),
		ForgejoScope:                strings.TrimSpace(os.Getenv("FORGEJO_RUNNER_SCOPE")),
		ForgejoAPITokenPath:         environmentOrDefault("FORGEJO_API_TOKEN_FILE", defaultAPITokenPath),
		ForgejoAllowInsecureHTTP:    strings.EqualFold(strings.TrimSpace(os.Getenv("FORGEJO_INSECURE_ALLOW_HTTP")), "true"),
		KubernetesAllowInsecureHTTP: strings.EqualFold(strings.TrimSpace(os.Getenv("KUBERNETES_INSECURE_ALLOW_HTTP")), "true"),
		Namespace:                   strings.TrimSpace(os.Getenv("POD_NAMESPACE")),
		RunnerName:                  strings.TrimSpace(os.Getenv("FORGEJO_RUNNER_NAME")),
		RunnerPodName:               environmentOrDefault("RUNNER_POD_NAME", defaultRunnerPodName),
		ControllerIdentity:          strings.TrimSpace(os.Getenv("POD_NAME")),
		LeaderLeaseName:             environmentOrDefault("LEADER_ELECTION_LEASE_NAME", defaultLeaderLeaseName),
		RunnerImage:                 strings.TrimSpace(os.Getenv("RUNNER_IMAGE")),
		CredentialSecretName:        environmentOrDefault("RUNNER_CREDENTIAL_SECRET_NAME", defaultCredentialName),
		PodTemplatePath:             environmentOrDefault("RUNNER_POD_TEMPLATE_FILE", defaultPodTemplatePath),
		KubernetesTokenPath:         environmentOrDefault("KUBERNETES_TOKEN_FILE", defaultKubernetesToken),
		KubernetesCAPath:            environmentOrDefault("KUBERNETES_CA_FILE", defaultKubernetesCA),
		PollInterval:                defaultPollInterval,
		RunnerStartupTimeout:        defaultRunnerStartupTimeout,
		RunnerUnknownTimeout:        defaultRunnerUnknownTimeout,
		MaxConcurrent:               defaultMaxConcurrent,
	}

	runnerLabels, err := parseRunnerLabels(os.Getenv("FORGEJO_RUNNER_LABELS"))
	if err != nil {
		return Config{}, err
	}
	cfg.RunnerLabels = runnerLabels

	if cfg.ForgejoURL == "" {
		return Config{}, errors.New("FORGEJO_INSTANCE_URL is required")
	}
	parsedURL, err := url.Parse(cfg.ForgejoURL)
	if err != nil {
		return Config{}, fmt.Errorf("parse FORGEJO_INSTANCE_URL: %w", err)
	}
	if parsedURL.Host == "" {
		return Config{}, errors.New("FORGEJO_INSTANCE_URL must include a host")
	}
	if parsedURL.User != nil {
		return Config{}, errors.New("FORGEJO_INSTANCE_URL must not contain user information")
	}
	if parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return Config{}, errors.New("FORGEJO_INSTANCE_URL must not contain a query or fragment")
	}
	if parsedURL.Scheme != "https" && (parsedURL.Scheme != "http" || !cfg.ForgejoAllowInsecureHTTP) {
		return Config{}, errors.New("FORGEJO_INSTANCE_URL must use HTTPS unless FORGEJO_INSECURE_ALLOW_HTTP=true")
	}
	if _, err := scopeAPIPath(cfg.ForgejoScope); err != nil {
		return Config{}, err
	}
	if cfg.Namespace == "" {
		return Config{}, errors.New("POD_NAMESPACE is required")
	}
	if cfg.RunnerName == "" {
		return Config{}, errors.New("FORGEJO_RUNNER_NAME is required")
	}
	if len(cfg.RunnerName) > 252 {
		return Config{}, errors.New("FORGEJO_RUNNER_NAME must be at most 252 characters")
	}
	if len(cfg.RunnerLabels) == 0 {
		return Config{}, errors.New("FORGEJO_RUNNER_LABELS must contain at least one label")
	}
	if cfg.ControllerIdentity == "" {
		return Config{}, errors.New("POD_NAME is required")
	}
	if len(cfg.ControllerIdentity) > 63 || !dnsLabel.MatchString(cfg.ControllerIdentity) {
		return Config{}, errors.New("POD_NAME must be a DNS label")
	}
	if cfg.RunnerImage == "" {
		return Config{}, errors.New("RUNNER_IMAGE is required")
	}
	if len(cfg.RunnerImage) > 512 || !imageReference.MatchString(cfg.RunnerImage) {
		return Config{}, errors.New("RUNNER_IMAGE contains unsupported characters")
	}
	for name, value := range map[string]string{
		"POD_NAMESPACE":                 cfg.Namespace,
		"RUNNER_POD_NAME":               cfg.RunnerPodName,
		"RUNNER_CREDENTIAL_SECRET_NAME": cfg.CredentialSecretName,
	} {
		if len(value) > 61 || !dnsLabel.MatchString(value) {
			return Config{}, fmt.Errorf("%s must be a DNS label", name)
		}
	}

	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if len(cfg.LeaderLeaseName) > 63 || !dnsLabel.MatchString(cfg.LeaderLeaseName) {
		return Config{}, errors.New("LEADER_ELECTION_LEASE_NAME must be a DNS label")
	}
	port := environmentOrDefault("KUBERNETES_SERVICE_PORT_HTTPS", "443")
	cfg.KubernetesAPIURL = strings.TrimSpace(os.Getenv("KUBERNETES_API_URL"))
	if cfg.KubernetesAPIURL == "" {
		if host == "" {
			return Config{}, errors.New("KUBERNETES_SERVICE_HOST is required")
		}
		cfg.KubernetesAPIURL = "https://" + net.JoinHostPort(host, port)
	}

	if raw := strings.TrimSpace(os.Getenv("POLL_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse POLL_INTERVAL: %w", err)
		}
		if parsed < 250*time.Millisecond {
			return Config{}, errors.New("POLL_INTERVAL must be at least 250ms")
		}
		cfg.PollInterval = parsed
	}
	for name, target := range map[string]*time.Duration{
		"RUNNER_STARTUP_TIMEOUT": &cfg.RunnerStartupTimeout,
		"RUNNER_UNKNOWN_TIMEOUT": &cfg.RunnerUnknownTimeout,
	} {
		if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return Config{}, fmt.Errorf("parse %s: %w", name, err)
			}
			if parsed <= 0 {
				return Config{}, fmt.Errorf("%s must be positive", name)
			}
			*target = parsed
		}
	}

	if raw := strings.TrimSpace(os.Getenv("MAX_CONCURRENT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse MAX_CONCURRENT: %w", err)
		}
		cfg.MaxConcurrent = parsed
	}
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > maxSupportedConcurrent {
		return Config{}, fmt.Errorf("MAX_CONCURRENT must be between 1 and %d", maxSupportedConcurrent)
	}
	return cfg, nil
}

func parseRunnerLabels(raw string) ([]string, error) {
	if strings.ContainsAny(raw, "\r\n\x00") {
		return nil, errors.New("FORGEJO_RUNNER_LABELS must not contain newline or NUL characters")
	}

	var labels []string
	seen := make(map[string]struct{})
	for index, spec := range strings.Split(raw, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return nil, fmt.Errorf("FORGEJO_RUNNER_LABELS entry %d is empty", index+1)
		}
		label, _, _ := strings.Cut(spec, ":")
		label = strings.TrimSpace(label)
		if label == "" {
			return nil, fmt.Errorf("FORGEJO_RUNNER_LABELS entry %d has an empty label name", index+1)
		}
		if _, duplicate := seen[label]; duplicate {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	return labels, nil
}

func environmentOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
