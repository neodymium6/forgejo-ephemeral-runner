package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"
)

func TestBaseRunnerPodTemplate(t *testing.T) {
	templateBytes, err := os.ReadFile("../../deploy/base/runner-pod.json")
	if err != nil {
		t.Fatalf("read Pod template: %v", err)
	}
	podTemplate, err := template.New("runner-pod").Option("missingkey=error").Parse(string(templateBytes))
	if err != nil {
		t.Fatalf("parse Pod template: %v", err)
	}
	data := runnerPodTemplateData{
		Namespace:            "forgejo-runners",
		PodName:              "runner-job",
		CredentialSecretName: "runner-credential",
		RunnerImage:          "registry.example.com/runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	var rendered bytes.Buffer
	if err := podTemplate.Execute(&rendered, data); err != nil {
		t.Fatalf("render Pod template: %v", err)
	}
	if strings.Contains(rendered.String(), "{{") {
		t.Fatal("rendered Pod contains an unresolved template expression")
	}

	var pod kubernetesPod
	if err := json.Unmarshal(rendered.Bytes(), &pod); err != nil {
		t.Fatalf("decode rendered Pod: %v", err)
	}
	if pod.Metadata.Name != data.PodName || pod.Metadata.Namespace != data.Namespace {
		t.Fatalf("rendered metadata = %s/%s", pod.Metadata.Namespace, pod.Metadata.Name)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("runner Pod automounts a service account token")
	}
	if pod.Spec.RestartPolicy != "Never" {
		t.Fatalf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}

	var raw map[string]any
	if err := json.Unmarshal(rendered.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	spec := raw["spec"].(map[string]any)
	containers := spec["containers"].([]any)
	runner := containers[0].(map[string]any)
	if runner["image"] != data.RunnerImage {
		t.Fatalf("runner image = %q, want %q", runner["image"], data.RunnerImage)
	}
}

func TestKubernetesCredentialUsesSlotAndStoresHandle(t *testing.T) {
	var received kubernetesSecret
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/secrets" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode Secret: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client := &KubernetesClient{
		httpClient:     server.Client(),
		podsURL:        server.URL + "/pods",
		secretsURL:     server.URL + "/secrets",
		namespace:      "forgejo-runners",
		runnerPodName:  "runner-job",
		credentialName: "runner-credential",
		runnerImage:    "runner:latest",
	}
	registration := Registration{ID: 42, UUID: "runner-uuid", Token: "runner-token"}
	if err := client.CreateCredential(context.Background(), 2, registration, "job-handle"); err != nil {
		t.Fatalf("CreateCredential() error = %v", err)
	}
	if received.Metadata.Name != "runner-credential-2" || received.Metadata.Namespace != "forgejo-runners" {
		t.Fatalf("Secret metadata = %s/%s", received.Metadata.Namespace, received.Metadata.Name)
	}
	if got := string(received.Data["handle"]); got != "job-handle" {
		t.Fatalf("handle = %q", got)
	}
	if got := string(received.Data["uuid"]); got != registration.UUID {
		t.Fatalf("uuid = %q", got)
	}
	if !received.Immutable {
		t.Fatal("runner credential Secret is mutable")
	}
}

func TestKubernetesServiceAccountTokenReloadsForAPIAndLeaderElection(t *testing.T) {
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("initial-token"), 0o600); err != nil {
		t.Fatalf("write initial token: %v", err)
	}

	var authorizations []string
	lease := kubernetesLease{}
	lease.Metadata.ResourceVersion = "1"
	lease.Spec.HolderIdentity = "another-controller"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/forgejo-runners/pods/"):
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/apis/coordination.k8s.io/v1/namespaces/forgejo-runners/leases/"):
			writeLease(t, w, lease, http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{
		KubernetesAPIURL:     server.URL,
		KubernetesTokenPath:  tokenPath,
		PodTemplatePath:      "../../deploy/base/runner-pod.json",
		Namespace:            "forgejo-runners",
		RunnerPodName:        "runner-job",
		CredentialSecretName: "runner-credential",
		RunnerImage:          "runner:latest",
		LeaderLeaseName:      "controller",
		ControllerIdentity:   "controller-0",
	}
	client, err := NewKubernetesClient(cfg)
	if err != nil {
		t.Fatalf("NewKubernetesClient() error = %v", err)
	}
	if _, err := client.GetPod(context.Background(), 0); err != nil {
		t.Fatalf("GetPod() error = %v", err)
	}

	if err := os.WriteFile(tokenPath, []byte("rotated-token"), 0o600); err != nil {
		t.Fatalf("write rotated token: %v", err)
	}
	elector, err := NewLeaderElector(cfg, client, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewLeaderElector() error = %v", err)
	}
	elector.now = func() time.Time {
		return time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	}
	if _, err := elector.tryAcquireOrRenew(context.Background()); err != nil {
		t.Fatalf("tryAcquireOrRenew() error = %v", err)
	}

	want := []string{"Bearer initial-token", "Bearer rotated-token"}
	if !slices.Equal(authorizations, want) {
		t.Fatalf("Authorization headers = %q, want %q", authorizations, want)
	}
}
