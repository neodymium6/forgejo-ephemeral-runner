package controller

import (
	"bytes"
	"encoding/json"
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
