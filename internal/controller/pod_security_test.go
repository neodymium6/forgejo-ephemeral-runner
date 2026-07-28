package controller

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"text/template"
)

func TestValidateRunnerPodTemplateRejectsUnsafeChanges(t *testing.T) {
	base, data := renderedBaseRunnerPod(t)
	tests := []struct {
		name       string
		wantError  string
		mutateFunc func(*kubernetesPod)
	}{
		{
			name:      "service account token automount",
			wantError: "automountServiceAccountToken",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.AutomountServiceAccountToken = testPointer(true)
			},
		},
		{
			name:      "unexpected service account",
			wantError: "service account",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.ServiceAccountName = "default"
			},
		},
		{
			name:      "host network",
			wantError: "host network",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.HostNetwork = true
			},
		},
		{
			name:      "shared process namespace",
			wantError: "process namespaces",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.ShareProcessNamespace = testPointer(true)
			},
		},
		{
			name:      "non-default seccomp",
			wantError: "RuntimeDefault",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.SecurityContext.SeccompProfile.Type = "Unconfined"
			},
		},
		{
			name:      "init container",
			wantError: "exactly one regular container",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.InitContainers = []kubernetesContainer{{Name: "init"}}
			},
		},
		{
			name:      "sidecar",
			wantError: "exactly one regular container",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers = append(pod.Spec.Containers, kubernetesContainer{Name: "sidecar"})
			},
		},
		{
			name:      "different image",
			wantError: "configured runner image",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers[0].Image = "registry.example.com/other:latest"
			},
		},
		{
			name:      "different command",
			wantError: "one-job launcher",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers[0].Command = []string{"sh"}
			},
		},
		{
			name:      "privileged container",
			wantError: "unprivileged",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers[0].SecurityContext.Privileged = testPointer(true)
			},
		},
		{
			name:      "privilege escalation",
			wantError: "privilege escalation",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = testPointer(true)
			},
		},
		{
			name:      "added capability",
			wantError: "drop ALL",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers[0].SecurityContext.Capabilities.Add = []string{"NET_ADMIN"}
			},
		},
		{
			name:      "host port",
			wantError: "host ports",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers[0].Ports = []kubernetesContainerPort{{HostPort: 8080}}
			},
		},
		{
			name:      "extra mount",
			wantError: "reviewed volume mounts",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, kubernetesVolumeMount{Name: "extra", MountPath: "/extra"})
			},
		},
		{
			name:      "bidirectional mount propagation",
			wantError: "mount propagation",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Containers[0].VolumeMounts[2].MountPropagation = testPointer("Bidirectional")
			},
		},
		{
			name:      "host path",
			wantError: "forbidden source",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Volumes[2].HostPath = json.RawMessage(`{"path":"/var/run"}`)
			},
		},
		{
			name:      "projected service account token",
			wantError: "forbidden source",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Volumes[2].Projected = json.RawMessage(`{"sources":[{"serviceAccountToken":{"path":"token"}}]}`)
			},
		},
		{
			name:      "persistent volume claim",
			wantError: "forbidden source",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Volumes[2].PersistentVolumeClaim = json.RawMessage(`{"claimName":"cache"}`)
			},
		},
		{
			name:      "different credential secret",
			wantError: "slot credential",
			mutateFunc: func(pod *kubernetesPod) {
				pod.Spec.Volumes[1].Secret.SecretName = "another-secret"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := cloneRunnerPod(t, base)
			test.mutateFunc(&pod)
			err := validateRunnerPodTemplate(pod, data)
			if err == nil {
				t.Fatal("validateRunnerPodTemplate() accepted an unsafe mutation")
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validateRunnerPodTemplate() error = %q, want substring %q", err, test.wantError)
			}
		})
	}
}

func renderedBaseRunnerPod(t *testing.T) (kubernetesPod, runnerPodTemplateData) {
	t.Helper()
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
		Slot:                 3,
		PodName:              "runner-job-3",
		CredentialSecretName: "runner-credential-3",
		RunnerImage:          "registry.example.com/runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	var rendered bytes.Buffer
	if err := podTemplate.Execute(&rendered, data); err != nil {
		t.Fatalf("render Pod template: %v", err)
	}
	var pod kubernetesPod
	if err := json.Unmarshal(rendered.Bytes(), &pod); err != nil {
		t.Fatalf("decode rendered Pod: %v", err)
	}
	return pod, data
}

func cloneRunnerPod(t *testing.T, pod kubernetesPod) kubernetesPod {
	t.Helper()
	encoded, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("encode Pod clone: %v", err)
	}
	var cloned kubernetesPod
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatalf("decode Pod clone: %v", err)
	}
	return cloned
}

func testPointer[T any](value T) *T {
	return &value
}
