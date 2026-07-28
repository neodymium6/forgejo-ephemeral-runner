package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"text/template"
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

	client := &kubernetesClient{
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
