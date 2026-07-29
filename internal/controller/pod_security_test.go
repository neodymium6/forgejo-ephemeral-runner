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
		mutateFunc func(*runnerPodTemplate)
	}{
		{
			name:      "service account token automount",
			wantError: "automountServiceAccountToken",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.AutomountServiceAccountToken = testPointer(true)
			},
		},
		{
			name:      "unexpected service account",
			wantError: "service account",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.ServiceAccountName = "default"
			},
		},
		{
			name:      "host network",
			wantError: "host network",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.HostNetwork = true
			},
		},
		{
			name:      "shared process namespace",
			wantError: "process namespaces",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.ShareProcessNamespace = testPointer(true)
			},
		},
		{
			name:      "non-default seccomp",
			wantError: "RuntimeDefault",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.SecurityContext.SeccompProfile.Type = "Unconfined"
			},
		},
		{
			name:      "init container",
			wantError: "exactly one regular container",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.InitContainers = []kubernetesContainer{{Name: "init"}}
			},
		},
		{
			name:      "sidecar",
			wantError: "exactly one regular container",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers = append(pod.Spec.Containers, kubernetesContainer{Name: "sidecar"})
			},
		},
		{
			name:      "different image",
			wantError: "configured runner image",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].Image = "registry.example.com/other:latest"
			},
		},
		{
			name:      "different command",
			wantError: "one-job launcher",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].Command = []string{"sh"}
			},
		},
		{
			name:      "privileged container",
			wantError: "unprivileged",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].SecurityContext.Privileged = testPointer(true)
			},
		},
		{
			name:      "privilege escalation",
			wantError: "privilege escalation",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = testPointer(true)
			},
		},
		{
			name:      "root user",
			wantError: "non-root execution identity",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].SecurityContext.RunAsUser = testPointer[int64](0)
			},
		},
		{
			name:      "root group",
			wantError: "non-root execution identity",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].SecurityContext.RunAsGroup = testPointer[int64](0)
			},
		},
		{
			name:      "non-root disabled",
			wantError: "non-root execution identity",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].SecurityContext.RunAsNonRoot = testPointer(false)
			},
		},
		{
			name:      "added capability",
			wantError: "drop ALL",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].SecurityContext.Capabilities.Add = []string{"NET_ADMIN"}
			},
		},
		{
			name:      "host port",
			wantError: "host ports",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].Ports = []kubernetesContainerPort{{HostPort: 8080}}
			},
		},
		{
			name:      "extra mount",
			wantError: "reviewed volume mounts",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, kubernetesVolumeMount{Name: "extra", MountPath: "/extra"})
			},
		},
		{
			name:      "bidirectional mount propagation",
			wantError: "mount propagation",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].VolumeMounts[2].MountPropagation = testPointer("Bidirectional")
			},
		},
		{
			name:      "host path",
			wantError: "forbidden source",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Volumes[2].HostPath = json.RawMessage(`{"path":"/var/run"}`)
			},
		},
		{
			name:      "projected service account token",
			wantError: "forbidden source",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Volumes[2].Projected = json.RawMessage(`{"sources":[{"serviceAccountToken":{"path":"token"}}]}`)
			},
		},
		{
			name:      "persistent volume claim",
			wantError: "forbidden source",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Volumes[2].PersistentVolumeClaim = json.RawMessage(`{"claimName":"cache"}`)
			},
		},
		{
			name:      "different credential secret",
			wantError: "slot credential",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Volumes[1].Secret.SecretName = "another-secret"
			},
		},
		{
			name:      "root-only credential mode",
			wantError: "mode 0440",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Volumes[1].Secret.DefaultMode = testPointer[int32](0o400)
			},
		},
		{
			name:      "extra label",
			wantError: "reviewed labels",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Metadata.Labels["example.invalid/extra"] = "true"
			},
		},
		{
			name:      "different termination grace period",
			wantError: "termination grace period",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.TerminationGracePeriod = testPointer[int64](60)
			},
		},
		{
			name:      "different filesystem group",
			wantError: "filesystem group",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.SecurityContext.FSGroup = testPointer[int64](0)
			},
		},
		{
			name:      "different image pull policy",
			wantError: "image pull policy",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].ImagePullPolicy = "Always"
			},
		},
		{
			name:      "different literal environment",
			wantError: "literal environment variables",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].Env[0].Value = "/root"
			},
		},
		{
			name:      "different environment ConfigMap",
			wantError: "settings ConfigMap",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Containers[0].EnvFrom[0].ConfigMapRef.Name = "other-settings"
			},
		},
		{
			name:      "missing resource limit",
			wantError: "resources",
			mutateFunc: func(pod *runnerPodTemplate) {
				delete(pod.Spec.Containers[0].Resources.Limits, "memory")
			},
		},
		{
			name:      "additional Secret volume source",
			wantError: "reviewed ConfigMap",
			mutateFunc: func(pod *runnerPodTemplate) {
				pod.Spec.Volumes[0].Secret = &kubernetesSecretVolumeSource{SecretName: "another-secret"}
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

func TestDecodeRunnerPodTemplateRejectsUnknownFields(t *testing.T) {
	base, _ := renderedBaseRunnerPod(t)
	baseJSON, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("encode base Pod: %v", err)
	}
	tests := []struct {
		name       string
		mutateFunc func(map[string]any)
	}{
		{
			name: "Secret envFrom reference",
			mutateFunc: func(pod map[string]any) {
				runner := rawRunnerContainer(pod)
				runner["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "another-secret"}}}
			},
		},
		{
			name: "Secret env valueFrom reference",
			mutateFunc: func(pod map[string]any) {
				runner := rawRunnerContainer(pod)
				environment := runner["env"].([]any)
				environment[0].(map[string]any)["valueFrom"] = map[string]any{
					"secretKeyRef": map[string]any{"name": "another-secret", "key": "token"},
				}
			},
		},
		{
			name: "container lifecycle",
			mutateFunc: func(pod map[string]any) {
				rawRunnerContainer(pod)["lifecycle"] = map[string]any{}
			},
		},
		{
			name: "Pod node selector",
			mutateFunc: func(pod map[string]any) {
				pod["spec"].(map[string]any)["nodeSelector"] = map[string]any{"example.invalid/node": "runner"}
			},
		},
		{
			name: "metadata annotations",
			mutateFunc: func(pod map[string]any) {
				pod["metadata"].(map[string]any)["annotations"] = map[string]any{"example.invalid/value": "true"}
			},
		},
		{
			name: "downward API volume",
			mutateFunc: func(pod map[string]any) {
				volumes := pod["spec"].(map[string]any)["volumes"].([]any)
				volumes[2].(map[string]any)["downwardAPI"] = map[string]any{}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var pod map[string]any
			if err := json.Unmarshal(baseJSON, &pod); err != nil {
				t.Fatal(err)
			}
			test.mutateFunc(pod)
			mutated, err := json.Marshal(pod)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeRunnerPodTemplate(mutated)
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("decodeRunnerPodTemplate() error = %v, want unknown field", err)
			}
		})
	}

	if _, err := decodeRunnerPodTemplate(append(baseJSON, []byte("\n{}")...)); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("decodeRunnerPodTemplate() trailing data error = %v", err)
	}
}

func rawRunnerContainer(pod map[string]any) map[string]any {
	spec := pod["spec"].(map[string]any)
	return spec["containers"].([]any)[0].(map[string]any)
}

func renderedBaseRunnerPod(t *testing.T) (runnerPodTemplate, runnerPodTemplateData) {
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
	pod, err := decodeRunnerPodTemplate(rendered.Bytes())
	if err != nil {
		t.Fatalf("decode rendered Pod: %v", err)
	}
	return pod, data
}

func cloneRunnerPod(t *testing.T, pod runnerPodTemplate) runnerPodTemplate {
	t.Helper()
	encoded, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("encode Pod clone: %v", err)
	}
	cloned, err := decodeRunnerPodTemplate(encoded)
	if err != nil {
		t.Fatalf("decode Pod clone: %v", err)
	}
	return cloned
}

func testPointer[T any](value T) *T {
	return &value
}
