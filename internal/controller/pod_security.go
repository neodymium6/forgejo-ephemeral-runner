package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const runnerServiceAccountName = "forgejo-runner-job"

type kubernetesPodSpec struct {
	AutomountServiceAccountToken *bool                        `json:"automountServiceAccountToken"`
	ServiceAccountName           string                       `json:"serviceAccountName,omitempty"`
	RestartPolicy                string                       `json:"restartPolicy"`
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
	SeccompProfile *kubernetesSeccompProfile `json:"seccompProfile,omitempty"`
}

type kubernetesSeccompProfile struct {
	Type string `json:"type"`
}

type kubernetesContainer struct {
	Name            string                              `json:"name"`
	Image           string                              `json:"image"`
	Command         []string                            `json:"command,omitempty"`
	Args            []string                            `json:"args,omitempty"`
	SecurityContext *kubernetesContainerSecurityContext `json:"securityContext,omitempty"`
	Ports           []kubernetesContainerPort           `json:"ports,omitempty"`
	VolumeMounts    []kubernetesVolumeMount             `json:"volumeMounts,omitempty"`
}

type kubernetesContainerSecurityContext struct {
	AllowPrivilegeEscalation *bool                   `json:"allowPrivilegeEscalation,omitempty"`
	Capabilities             *kubernetesCapabilities `json:"capabilities,omitempty"`
	Privileged               *bool                   `json:"privileged,omitempty"`
	ProcMount                string                  `json:"procMount,omitempty"`
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
	EmptyDir              json.RawMessage                  `json:"emptyDir,omitempty"`
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

func validateRunnerPodTemplate(pod kubernetesPod, data runnerPodTemplateData) error {
	if pod.APIVersion != "v1" || pod.Kind != "Pod" {
		return errors.New("runner Pod template must define a v1 Pod")
	}
	if pod.Metadata.Name != data.PodName || pod.Metadata.Namespace != data.Namespace {
		return errors.New("runner Pod template metadata does not match controller configuration")
	}
	if pod.Metadata.Labels[managedByLabel] != managedByValue || pod.Metadata.Labels[slotLabel] != fmt.Sprintf("%d", data.Slot) {
		return errors.New("runner Pod template does not contain the expected ownership labels")
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
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC || booleanValue(pod.Spec.ShareProcessNamespace) {
		return errors.New("runner Pod template must not share host network, PID, IPC, or process namespaces")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
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
	if !slices.Equal(container.Command, []string{"/bin/forgejo-ephemeral-one-job"}) || len(container.Args) != 0 {
		return errors.New("runner Pod template must invoke only the one-job launcher")
	}
	security := container.SecurityContext
	if security == nil || booleanValue(security.Privileged) {
		return errors.New("runner container must explicitly remain unprivileged")
	}
	if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
		return errors.New("runner container must disable privilege escalation")
	}
	if security.Capabilities == nil || len(security.Capabilities.Add) != 0 || !slices.Equal(security.Capabilities.Drop, []string{"ALL"}) {
		return errors.New("runner container must add no capabilities and drop ALL")
	}
	if security.ProcMount != "" && security.ProcMount != "Default" {
		return errors.New("runner container must use the default proc mount")
	}
	if security.RunAsUser == nil || *security.RunAsUser != 0 || security.RunAsNonRoot == nil || *security.RunAsNonRoot {
		return errors.New("runner container must use the reviewed root-in-container execution model")
	}
	for _, port := range container.Ports {
		if port.HostPort != 0 {
			return errors.New("runner container must not request host ports")
		}
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
			if volume.ConfigMap == nil || volume.ConfigMap.Name != "forgejo-runner-config" {
				return errors.New("runner-config must use the reviewed ConfigMap")
			}
		case "runner-credential":
			if volume.Secret == nil || volume.Secret.SecretName != credentialName || volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0o400 {
				return errors.New("runner-credential must use only the slot credential with mode 0400")
			}
		case "workspace", "home", "tmp":
			if len(volume.EmptyDir) == 0 {
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
