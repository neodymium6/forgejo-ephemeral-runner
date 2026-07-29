package controller

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func setValidConfigEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("FORGEJO_INSTANCE_URL", "https://forgejo.example.com")
	t.Setenv("FORGEJO_REPOSITORY_ALLOWLIST", "example/project")
	t.Setenv("POD_NAMESPACE", "forgejo-runners")
	t.Setenv("FORGEJO_RUNNER_NAME", "kubernetes-ephemeral")
	t.Setenv("POD_NAME", "controller-0")
	t.Setenv("FORGEJO_RUNNER_LABELS", "linux-amd64:host, nix")
	t.Setenv("RUNNER_IMAGE", "registry.example.com/runner:latest")
	t.Setenv("KUBERNETES_API_URL", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "192.0.2.1")
	t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")
}

func TestConfigParsesRunnerCapacityAndLabels(t *testing.T) {
	setValidConfigEnvironment(t)
	t.Setenv("MAX_CONCURRENT", "3")
	t.Setenv("KUBERNETES_INSECURE_ALLOW_HTTP", "true")
	t.Setenv("RUNNER_STARTUP_TIMEOUT", "45m")
	t.Setenv("RUNNER_UNKNOWN_TIMEOUT", "7m")

	cfg, err := ConfigFromEnvironment()
	if err != nil {
		t.Fatalf("ConfigFromEnvironment() error = %v", err)
	}
	if cfg.MaxConcurrent != 3 {
		t.Fatalf("MaxConcurrent = %d, want 3", cfg.MaxConcurrent)
	}
	if !cfg.KubernetesAllowInsecureHTTP {
		t.Fatal("KUBERNETES_INSECURE_ALLOW_HTTP=true was not parsed")
	}
	if cfg.RunnerStartupTimeout != 45*time.Minute || cfg.RunnerUnknownTimeout != 7*time.Minute {
		t.Fatalf("runner timeouts = %s and %s", cfg.RunnerStartupTimeout, cfg.RunnerUnknownTimeout)
	}
	if want := []string{"linux-amd64", "nix"}; !reflect.DeepEqual(cfg.RunnerLabels, want) {
		t.Fatalf("RunnerLabels = %v, want %v", cfg.RunnerLabels, want)
	}
	if want := []string{"example/project"}; !reflect.DeepEqual(cfg.ForgejoRepositoryAllowlist, want) {
		t.Fatalf("ForgejoRepositoryAllowlist = %v, want %v", cfg.ForgejoRepositoryAllowlist, want)
	}
}

func TestParseRepositoryAllowlist(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "multiple repositories", raw: "example/one\r\nexample/two\n", want: []string{"example/one", "example/two"}},
		{name: "wildcard", raw: "  *\n", want: []string{"*"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRepositoryAllowlist(test.raw)
			if err != nil {
				t.Fatalf("parseRepositoryAllowlist() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseRepositoryAllowlist() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestParseRepositoryAllowlistRejectsMalformedInput(t *testing.T) {
	for _, value := range []string{
		"",
		"example",
		"a/b/c",
		"./repo",
		"../repo",
		"owner/.",
		"owner/..",
		"example /project",
		"example/project\nexample/project",
		"*\nexample/project",
		"example/project\n*",
		"\x00",
	} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			if _, err := parseRepositoryAllowlist(value); err == nil {
				t.Fatalf("parseRepositoryAllowlist(%q) succeeded", value)
			}
		})
	}
}

func TestParseRunnerLabelsRejectsMalformedInput(t *testing.T) {
	for _, value := range []string{
		"",
		"linux-amd64:host,,nix",
		",linux-amd64:host",
		"linux-amd64:host,",
		":host",
		"linux-amd64:host\nnix",
		"linux-amd64:host\rnix",
		"linux-amd64:host\x00nix",
	} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			if _, err := parseRunnerLabels(value); err == nil {
				t.Fatalf("parseRunnerLabels(%q) succeeded", value)
			}
		})
	}
}

func TestConfigRejectsMalformedRunnerLabels(t *testing.T) {
	setValidConfigEnvironment(t)
	t.Setenv("FORGEJO_RUNNER_LABELS", "linux-amd64:host,,nix")
	if _, err := ConfigFromEnvironment(); err == nil {
		t.Fatal("ConfigFromEnvironment() accepted an empty runner label")
	}
}

func TestConfigRejectsInvalidRunnerTimeouts(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "RUNNER_STARTUP_TIMEOUT", value: "0s"},
		{name: "RUNNER_STARTUP_TIMEOUT", value: "invalid"},
		{name: "RUNNER_UNKNOWN_TIMEOUT", value: "-1s"},
		{name: "RUNNER_UNKNOWN_TIMEOUT", value: "invalid"},
	}
	for _, test := range tests {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			setValidConfigEnvironment(t)
			t.Setenv(test.name, test.value)
			if _, err := ConfigFromEnvironment(); err == nil {
				t.Fatalf("%s=%q succeeded", test.name, test.value)
			}
		})
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
