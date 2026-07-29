package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

const runnerServiceAccountName = "forgejo-runner-job"

const runnerUserID int64 = 65532

type runnerPodTemplate struct {
	APIVersion string                    `json:"apiVersion"`
	Kind       string                    `json:"kind"`
	Metadata   runnerPodTemplateMetadata `json:"metadata"`
	Spec       kubernetesPodSpec         `json:"spec"`
}

type runnerPodTemplateMetadata struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

type kubernetesPodSpec struct {
	AutomountServiceAccountToken *bool                        `json:"automountServiceAccountToken"`
	ServiceAccountName           string                       `json:"serviceAccountName,omitempty"`
	RestartPolicy                string                       `json:"restartPolicy"`
	TerminationGracePeriod       *int64                       `json:"terminationGracePeriodSeconds,omitempty"`
	HostNetwork                  bool                         `json:"hostNetwork,omitempty"`
	HostPID                      bool                         `json:"hostPID,omitempty"`
	HostIPC                      bool                         `json:"hostIPC,omitempty"`
	ShareProcessNamespace        *bool                        `json:"shareProcessNamespace,omitempty"`
	SecurityContext              kubernetesPodSecurityContext `json:"securityContext,omitempty"`
	InitContainers               []kubernetesContainer        `json:"initContainers,omitempty"`
	Containers                   []kubernetesContainer        `json:"containers"`
	EphemeralContainers          []kubernetesContainer        `json:"ephemeralContainers,omitempty"`
	Volumes                      []kubernetesVolume           `json:"volumes,omitempty"`
}

type kubernetesPodSecurityContext struct {
	FSGroup             *int64                    `json:"fsGroup,omitempty"`
	FSGroupChangePolicy string                    `json:"fsGroupChangePolicy,omitempty"`
	SeccompProfile      *kubernetesSeccompProfile `json:"seccompProfile,omitempty"`
}

type kubernetesSeccompProfile struct {
	Type string `json:"type"`
}

type kubernetesContainer struct {
	Name            string                              `json:"name"`
	Image           string                              `json:"image"`
	ImagePullPolicy string                              `json:"imagePullPolicy,omitempty"`
	Command         []string                            `json:"command,omitempty"`
	Args            []string                            `json:"args,omitempty"`
	Env             []kubernetesEnvVar                  `json:"env,omitempty"`
	EnvFrom         []kubernetesEnvFromSource           `json:"envFrom,omitempty"`
	Resources       kubernetesResourceRequirements      `json:"resources,omitempty"`
	SecurityContext *kubernetesContainerSecurityContext `json:"securityContext,omitempty"`
	Ports           []kubernetesContainerPort           `json:"ports,omitempty"`
	VolumeMounts    []kubernetesVolumeMount             `json:"volumeMounts,omitempty"`
}

type kubernetesContainerSecurityContext struct {
	AllowPrivilegeEscalation *bool                   `json:"allowPrivilegeEscalation,omitempty"`
	Capabilities             *kubernetesCapabilities `json:"capabilities,omitempty"`
	Privileged               *bool                   `json:"privileged,omitempty"`
	ProcMount                string                  `json:"procMount,omitempty"`
	ReadOnlyRootFilesystem   *bool                   `json:"readOnlyRootFilesystem,omitempty"`
	RunAsGroup               *int64                  `json:"runAsGroup,omitempty"`
	RunAsNonRoot             *bool                   `json:"runAsNonRoot,omitempty"`
	RunAsUser                *int64                  `json:"runAsUser,omitempty"`
}

type kubernetesCapabilities struct {
	Add  []string `json:"add,omitempty"`
	Drop []string `json:"drop,omitempty"`
}

type kubernetesContainerPort struct {
	HostPort int32 `json:"hostPort,omitempty"`
}

type kubernetesEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type kubernetesEnvFromSource struct {
	Prefix       string                          `json:"prefix,omitempty"`
	ConfigMapRef *kubernetesLocalObjectReference `json:"configMapRef,omitempty"`
}

type kubernetesLocalObjectReference struct {
	Name string `json:"name"`
}

type kubernetesResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type kubernetesVolumeMount struct {
	Name             string  `json:"name"`
	MountPath        string  `json:"mountPath"`
	ReadOnly         bool    `json:"readOnly,omitempty"`
	MountPropagation *string `json:"mountPropagation,omitempty"`
}

type kubernetesVolume struct {
	Name                  string                           `json:"name"`
	ConfigMap             *kubernetesConfigMapVolumeSource `json:"configMap,omitempty"`
	Secret                *kubernetesSecretVolumeSource    `json:"secret,omitempty"`
	EmptyDir              *kubernetesEmptyDirVolumeSource  `json:"emptyDir,omitempty"`
	HostPath              json.RawMessage                  `json:"hostPath,omitempty"`
	Projected             json.RawMessage                  `json:"projected,omitempty"`
	PersistentVolumeClaim json.RawMessage                  `json:"persistentVolumeClaim,omitempty"`
	CSI                   json.RawMessage                  `json:"csi,omitempty"`
}

type kubernetesConfigMapVolumeSource struct {
	Name string `json:"name"`
}

type kubernetesSecretVolumeSource struct {
	SecretName  string `json:"secretName"`
	DefaultMode *int32 `json:"defaultMode,omitempty"`
}

type kubernetesEmptyDirVolumeSource struct {
	SizeLimit string `json:"sizeLimit"`
}

func decodeRunnerPodTemplate(data []byte) (runnerPodTemplate, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var pod runnerPodTemplate
	if err := decoder.Decode(&pod); err != nil {
		return runnerPodTemplate{}, fmt.Errorf("strictly decode runner Pod template: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return runnerPodTemplate{}, errors.New("runner Pod template contains trailing JSON data")
		}
		return runnerPodTemplate{}, fmt.Errorf("decode trailing runner Pod template data: %w", err)
	}
	return pod, nil
}

func validateRunnerPodTemplate(pod runnerPodTemplate, data runnerPodTemplateData) error {
	if pod.APIVersion != "v1" || pod.Kind != "Pod" {
		return errors.New("runner Pod template must define a v1 Pod")
	}
	if pod.Metadata.Name != data.PodName || pod.Metadata.Namespace != data.Namespace {
		return errors.New("runner Pod template metadata does not match controller configuration")
	}
	expectedLabels := map[string]string{
		"app.kubernetes.io/name":      "forgejo-ephemeral-runner",
		"app.kubernetes.io/component": "job",
		managedByLabel:                managedByValue,
		slotLabel:                     fmt.Sprintf("%d", data.Slot),
	}
	if !maps.Equal(pod.Metadata.Labels, expectedLabels) {
		return errors.New("runner Pod template must contain exactly the reviewed labels")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		return errors.New("runner Pod template must set automountServiceAccountToken to false")
	}
	if pod.Spec.ServiceAccountName != runnerServiceAccountName {
		return fmt.Errorf("runner Pod template must use service account %q", runnerServiceAccountName)
	}
	if pod.Spec.RestartPolicy != "Never" {
		return errors.New("runner Pod template must set restartPolicy to Never")
	}
	if pod.Spec.TerminationGracePeriod == nil || *pod.Spec.TerminationGracePeriod != 30 {
		return errors.New("runner Pod template must use the reviewed 30-second termination grace period")
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC || booleanValue(pod.Spec.ShareProcessNamespace) {
		return errors.New("runner Pod template must not share host network, PID, IPC, or process namespaces")
	}
	podSecurity := pod.Spec.SecurityContext
	if podSecurity.FSGroup == nil || *podSecurity.FSGroup != runnerUserID || podSecurity.FSGroupChangePolicy != "OnRootMismatch" {
		return errors.New("runner Pod template must use the reviewed filesystem group")
	}
	if podSecurity.SeccompProfile == nil || podSecurity.SeccompProfile.Type != "RuntimeDefault" {
		return errors.New("runner Pod template must use the RuntimeDefault seccomp profile")
	}
	if len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Spec.Containers) != 1 {
		return errors.New("runner Pod template must contain exactly one regular container and no init or ephemeral containers")
	}
	if err := validateRunnerContainer(pod.Spec.Containers[0], data.RunnerImage); err != nil {
		return err
	}
	return validateRunnerVolumes(pod.Spec.Volumes, data.CredentialSecretName)
}

func validateRunnerContainer(container kubernetesContainer, runnerImage string) error {
	if container.Name != "runner" || container.Image != runnerImage {
		return errors.New("runner Pod template must contain only the configured runner image")
	}
	if container.ImagePullPolicy != "IfNotPresent" {
		return errors.New("runner container must use the reviewed image pull policy")
	}
	if !slices.Equal(container.Command, []string{"/bin/forgejo-ephemeral-one-job"}) || len(container.Args) != 0 {
		return errors.New("runner Pod template must invoke only the one-job launcher")
	}
	wantEnvFrom := []kubernetesEnvFromSource{{ConfigMapRef: &kubernetesLocalObjectReference{Name: "forgejo-runner-settings"}}}
	if !slices.EqualFunc(container.EnvFrom, wantEnvFrom, func(left, right kubernetesEnvFromSource) bool {
		return left.Prefix == right.Prefix && left.ConfigMapRef != nil && right.ConfigMapRef != nil && left.ConfigMapRef.Name == right.ConfigMapRef.Name
	}) {
		return errors.New("runner container must import only the reviewed settings ConfigMap")
	}
	wantEnv := []kubernetesEnvVar{
		{Name: "HOME", Value: "/home/runner"},
		{Name: "TMPDIR", Value: "/tmp"},
		{Name: "NIX_CONFIG", Value: "experimental-features = nix-command flakes\nsandbox = false\n"},
	}
	if !slices.Equal(container.Env, wantEnv) {
		return errors.New("runner container must use only the reviewed literal environment variables")
	}
	if err := validateResourceRequirements(container.Resources); err != nil {
		return err
	}
	security := container.SecurityContext
	if security == nil || booleanValue(security.Privileged) {
		return errors.New("runner container must explicitly remain unprivileged")
	}
	if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
		return errors.New("runner container must disable privilege escalation")
	}
	if security.ReadOnlyRootFilesystem == nil || *security.ReadOnlyRootFilesystem {
		return errors.New("runner container must use the reviewed writable image layer")
	}
	if security.Capabilities == nil || len(security.Capabilities.Add) != 0 || !slices.Equal(security.Capabilities.Drop, []string{"ALL"}) {
		return errors.New("runner container must add no capabilities and drop ALL")
	}
	if security.ProcMount != "" && security.ProcMount != "Default" {
		return errors.New("runner container must use the default proc mount")
	}
	if security.RunAsUser == nil || *security.RunAsUser != runnerUserID ||
		security.RunAsGroup == nil || *security.RunAsGroup != runnerUserID ||
		security.RunAsNonRoot == nil || !*security.RunAsNonRoot {
		return errors.New("runner container must use the reviewed non-root execution identity")
	}
	if len(container.Ports) != 0 {
		return errors.New("runner container must not expose container or host ports")
	}

	expectedMounts := map[string]kubernetesVolumeMount{
		"runner-config":     {Name: "runner-config", MountPath: "/etc/forgejo-runner", ReadOnly: true},
		"runner-credential": {Name: "runner-credential", MountPath: "/run/secrets/forgejo-runner", ReadOnly: true},
		"workspace":         {Name: "workspace", MountPath: "/workspace"},
		"home":              {Name: "home", MountPath: "/home/runner"},
		"tmp":               {Name: "tmp", MountPath: "/tmp"},
	}
	if len(container.VolumeMounts) != len(expectedMounts) {
		return errors.New("runner container must use only the reviewed volume mounts")
	}
	for _, mount := range container.VolumeMounts {
		expected, ok := expectedMounts[mount.Name]
		if !ok || mount.MountPath != expected.MountPath || mount.ReadOnly != expected.ReadOnly {
			return fmt.Errorf("runner container volume mount %q is not approved", mount.Name)
		}
		if mount.MountPropagation != nil && *mount.MountPropagation != "None" {
			return fmt.Errorf("runner container volume mount %q uses mount propagation", mount.Name)
		}
		delete(expectedMounts, mount.Name)
	}
	if len(expectedMounts) != 0 {
		return errors.New("runner container is missing a reviewed volume mount")
	}
	return nil
}

func validateResourceRequirements(resources kubernetesResourceRequirements) error {
	wantKeys := map[string]struct{}{
		"cpu":               {},
		"memory":            {},
		"ephemeral-storage": {},
	}
	for name, values := range map[string]map[string]string{"requests": resources.Requests, "limits": resources.Limits} {
		if len(values) != len(wantKeys) {
			return fmt.Errorf("runner container resources must set only cpu, memory, and ephemeral-storage %s", name)
		}
		for key := range wantKeys {
			if strings.TrimSpace(values[key]) == "" {
				return fmt.Errorf("runner container resource %s.%s must be set", name, key)
			}
		}
	}
	return nil
}

func validateRunnerVolumes(volumes []kubernetesVolume, credentialName string) error {
	if len(volumes) != 5 {
		return errors.New("runner Pod template must use only the five reviewed volumes")
	}
	seen := make(map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		if _, duplicate := seen[volume.Name]; duplicate {
			return fmt.Errorf("runner Pod template contains duplicate volume %q", volume.Name)
		}
		seen[volume.Name] = struct{}{}
		if len(volume.HostPath) != 0 || len(volume.Projected) != 0 || len(volume.PersistentVolumeClaim) != 0 || len(volume.CSI) != 0 {
			return fmt.Errorf("runner Pod template volume %q uses a forbidden source", volume.Name)
		}
		switch volume.Name {
		case "runner-config":
			if volume.ConfigMap == nil || volume.ConfigMap.Name != "forgejo-runner-config" || volume.Secret != nil || volume.EmptyDir != nil {
				return errors.New("runner-config must use the reviewed ConfigMap")
			}
		case "runner-credential":
			if volume.Secret == nil || volume.Secret.SecretName != credentialName || volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0o440 || volume.ConfigMap != nil || volume.EmptyDir != nil {
				return errors.New("runner-credential must use only the slot credential with mode 0440")
			}
		case "workspace", "home", "tmp":
			if volume.EmptyDir == nil || strings.TrimSpace(volume.EmptyDir.SizeLimit) == "" || volume.ConfigMap != nil || volume.Secret != nil {
				return fmt.Errorf("runner Pod template volume %q must use emptyDir", volume.Name)
			}
		default:
			return fmt.Errorf("runner Pod template volume %q is not approved", volume.Name)
		}
	}
	return nil
}

func booleanValue(value *bool) bool {
	return value != nil && *value
}
