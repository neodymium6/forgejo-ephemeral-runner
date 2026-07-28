package controller

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	defaultAPITokenPath        = "/run/secrets/forgejo-api/api-token"
	defaultKubernetesToken     = "/var/run/secrets/forgejo-controller/token"
	defaultKubernetesCA        = "/var/run/secrets/forgejo-controller/ca.crt"
	defaultPodTemplatePath     = "/etc/forgejo-controller/runner-pod.json"
	defaultRunnerPodName       = "forgejo-ephemeral-runner-job"
	defaultCredentialName      = "forgejo-ephemeral-runner-credential"
	defaultPollInterval        = 2 * time.Second
	defaultControllerUserAgent = "forgejo-ephemeral-runner-controller"
)

var (
	dnsLabel       = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	imageReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
)

type Config struct {
	ForgejoURL           string
	ForgejoScope         string
	ForgejoAPITokenPath  string
	AllowInsecureHTTP    bool
	Namespace            string
	RunnerName           string
	RunnerPodName        string
	RunnerImage          string
	CredentialSecretName string
	PodTemplatePath      string
	KubernetesAPIURL     string
	KubernetesTokenPath  string
	KubernetesCAPath     string
	PollInterval         time.Duration
}

func ConfigFromEnvironment() (Config, error) {
	cfg := Config{
		ForgejoURL:           strings.TrimSpace(os.Getenv("FORGEJO_INSTANCE_URL")),
		ForgejoScope:         strings.TrimSpace(os.Getenv("FORGEJO_RUNNER_SCOPE")),
		ForgejoAPITokenPath:  environmentOrDefault("FORGEJO_API_TOKEN_FILE", defaultAPITokenPath),
		AllowInsecureHTTP:    strings.EqualFold(strings.TrimSpace(os.Getenv("FORGEJO_INSECURE_ALLOW_HTTP")), "true"),
		Namespace:            strings.TrimSpace(os.Getenv("POD_NAMESPACE")),
		RunnerName:           strings.TrimSpace(os.Getenv("FORGEJO_RUNNER_NAME")),
		RunnerPodName:        environmentOrDefault("RUNNER_POD_NAME", defaultRunnerPodName),
		RunnerImage:          strings.TrimSpace(os.Getenv("RUNNER_IMAGE")),
		CredentialSecretName: environmentOrDefault("RUNNER_CREDENTIAL_SECRET_NAME", defaultCredentialName),
		PodTemplatePath:      environmentOrDefault("RUNNER_POD_TEMPLATE_FILE", defaultPodTemplatePath),
		KubernetesTokenPath:  environmentOrDefault("KUBERNETES_TOKEN_FILE", defaultKubernetesToken),
		KubernetesCAPath:     environmentOrDefault("KUBERNETES_CA_FILE", defaultKubernetesCA),
		PollInterval:         defaultPollInterval,
	}

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
	if parsedURL.Scheme != "https" && !(parsedURL.Scheme == "http" && cfg.AllowInsecureHTTP) {
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
		if len(value) > 63 || !dnsLabel.MatchString(value) {
			return Config{}, fmt.Errorf("%s must be a DNS label", name)
		}
	}

	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
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
	return cfg, nil
}

func environmentOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
