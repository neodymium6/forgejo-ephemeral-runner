package controller

import (
	"reflect"
	"testing"
)

func setValidConfigEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("FORGEJO_INSTANCE_URL", "https://forgejo.example.com")
	t.Setenv("FORGEJO_RUNNER_SCOPE", "repository:example/project")
	t.Setenv("POD_NAMESPACE", "forgejo-runners")
	t.Setenv("FORGEJO_RUNNER_NAME", "kubernetes-ephemeral")
	t.Setenv("FORGEJO_RUNNER_LABELS", "linux-amd64:host, nix")
	t.Setenv("RUNNER_IMAGE", "registry.example.com/runner:latest")
	t.Setenv("KUBERNETES_API_URL", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "192.0.2.1")
	t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")
}

func TestConfigParsesRunnerCapacityAndLabels(t *testing.T) {
	setValidConfigEnvironment(t)
	t.Setenv("MAX_CONCURRENT", "3")

	cfg, err := ConfigFromEnvironment()
	if err != nil {
		t.Fatalf("ConfigFromEnvironment() error = %v", err)
	}
	if cfg.MaxConcurrent != 3 {
		t.Fatalf("MaxConcurrent = %d, want 3", cfg.MaxConcurrent)
	}
	if want := []string{"linux-amd64", "nix"}; !reflect.DeepEqual(cfg.RunnerLabels, want) {
		t.Fatalf("RunnerLabels = %v, want %v", cfg.RunnerLabels, want)
	}
}

func TestConfigRejectsUnsupportedRunnerCapacity(t *testing.T) {
	for _, value := range []string{"0", "11", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			setValidConfigEnvironment(t)
			t.Setenv("MAX_CONCURRENT", value)
			if _, err := ConfigFromEnvironment(); err == nil {
				t.Fatalf("MAX_CONCURRENT=%q succeeded", value)
			}
		})
	}
}
