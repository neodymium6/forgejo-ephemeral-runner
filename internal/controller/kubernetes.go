package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const runnerIDAnnotation = "forgejo-ephemeral-runner.dev/runner-id"

type kubernetesClient struct {
	httpClient     *http.Client
	podsURL        string
	secretsURL     string
	token          string
	podTemplate    *template.Template
	namespace      string
	runnerPodName  string
	credentialName string
	runnerImage    string
}

type runnerPodTemplateData struct {
	Namespace            string
	PodName              string
	CredentialSecretName string
	RunnerImage          string
}

type kubernetesPod struct {
	Metadata struct {
		Name              string  `json:"name"`
		Namespace         string  `json:"namespace"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
		RestartPolicy                string `json:"restartPolicy"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type kubernetesSecret struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Metadata   struct {
		Name        string            `json:"name,omitempty"`
		Namespace   string            `json:"namespace,omitempty"`
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Immutable bool              `json:"immutable,omitempty"`
	Type      string            `json:"type,omitempty"`
	Data      map[string][]byte `json:"data,omitempty"`
}

func NewKubernetesClient(cfg Config) (Kubernetes, error) {
	baseURL, err := url.Parse(cfg.KubernetesAPIURL)
	if err != nil {
		return nil, fmt.Errorf("parse Kubernetes API URL: %w", err)
	}
	if baseURL.Scheme != "https" && baseURL.Scheme != "http" {
		return nil, fmt.Errorf("unsupported Kubernetes API URL scheme %q", baseURL.Scheme)
	}

	tokenBytes, err := os.ReadFile(cfg.KubernetesTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service account token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, errors.New("Kubernetes service account token is empty")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if baseURL.Scheme == "https" {
		caBytes, err := os.ReadFile(cfg.KubernetesCAPath)
		if err != nil {
			return nil, fmt.Errorf("read Kubernetes service account CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("Kubernetes service account CA contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}

	templateBytes, err := os.ReadFile(cfg.PodTemplatePath)
	if err != nil {
		return nil, fmt.Errorf("read runner Pod template: %w", err)
	}
	podTemplate, err := template.New("runner-pod").Option("missingkey=error").Parse(string(templateBytes))
	if err != nil {
		return nil, fmt.Errorf("parse runner Pod template: %w", err)
	}

	apiBase := *baseURL
	apiBase.Path = "/api/v1/namespaces/" + url.PathEscape(cfg.Namespace)
	apiBase.RawQuery = ""
	apiBase.Fragment = ""
	podsURL := apiBase.String() + "/pods"
	secretsURL := apiBase.String() + "/secrets"

	return &kubernetesClient{
		httpClient: &http.Client{
			Transport:     transport,
			Timeout:       15 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
		podsURL:        podsURL,
		secretsURL:     secretsURL,
		token:          token,
		podTemplate:    podTemplate,
		namespace:      cfg.Namespace,
		runnerPodName:  cfg.RunnerPodName,
		credentialName: cfg.CredentialSecretName,
		runnerImage:    cfg.RunnerImage,
	}, nil
}

func (c *kubernetesClient) slotResources(slot int) (runnerPodTemplateData, string, string, error) {
	if slot < 0 || slot >= maxSupportedConcurrent {
		return runnerPodTemplateData{}, "", "", fmt.Errorf("runner slot must be between 0 and %d", maxSupportedConcurrent-1)
	}
	podName := resourceNameForSlot(c.runnerPodName, slot)
	credentialName := resourceNameForSlot(c.credentialName, slot)
	return runnerPodTemplateData{
		Namespace:            c.namespace,
		PodName:              podName,
		CredentialSecretName: credentialName,
		RunnerImage:          c.runnerImage,
	}, c.podsURL + "/" + url.PathEscape(podName), c.secretsURL + "/" + url.PathEscape(credentialName), nil
}

func resourceNameForSlot(base string, slot int) string {
	return fmt.Sprintf("%s-%d", base, slot)
}

func (c *kubernetesClient) GetPod(ctx context.Context, slot int) (PodState, error) {
	_, podURL, _, err := c.slotResources(slot)
	if err != nil {
		return PodState{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, podURL, nil)
	if err != nil {
		return PodState{}, err
	}
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PodState{}, fmt.Errorf("get pod: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return PodState{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return PodState{}, kubernetesResponseError("get pod", resp)
	}

	var pod kubernetesPod
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pod); err != nil {
		return PodState{}, fmt.Errorf("decode pod: %w", err)
	}
	return PodState{
		Exists:   true,
		Deleting: pod.Metadata.DeletionTimestamp != nil,
		Phase:    pod.Status.Phase,
	}, nil
}

func (c *kubernetesClient) CreatePod(ctx context.Context, slot int) error {
	templateData, _, _, err := c.slotResources(slot)
	if err != nil {
		return err
	}
	var rendered bytes.Buffer
	if err := c.podTemplate.Execute(&rendered, templateData); err != nil {
		return fmt.Errorf("render runner Pod template: %w", err)
	}
	var pod kubernetesPod
	if err := json.Unmarshal(rendered.Bytes(), &pod); err != nil {
		return fmt.Errorf("decode rendered runner Pod template: %w", err)
	}
	if pod.Metadata.Name != templateData.PodName || pod.Metadata.Namespace != templateData.Namespace {
		return errors.New("runner Pod template metadata does not match controller configuration")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		return errors.New("runner Pod template must set automountServiceAccountToken to false")
	}
	if pod.Spec.RestartPolicy != "Never" {
		return errors.New("runner Pod template must set restartPolicy to Never")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.podsURL, bytes.NewReader(rendered.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("create pod: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return kubernetesResponseError("create pod", resp)
	}
	return nil
}

func (c *kubernetesClient) DeletePod(ctx context.Context, slot int) error {
	_, podURL, _, err := c.slotResources(slot)
	if err != nil {
		return err
	}
	return c.delete(ctx, podURL, "delete pod")
}

func (c *kubernetesClient) GetCredential(ctx context.Context, slot int) (CredentialState, error) {
	_, _, credentialURL, err := c.slotResources(slot)
	if err != nil {
		return CredentialState{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, credentialURL, nil)
	if err != nil {
		return CredentialState{}, err
	}
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CredentialState{}, fmt.Errorf("get credential: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return CredentialState{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return CredentialState{}, kubernetesResponseError("get credential", resp)
	}

	var secret kubernetesSecret
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&secret); err != nil {
		return CredentialState{}, fmt.Errorf("decode credential: %w", err)
	}
	runnerID, err := parseRunnerID(secret.Metadata.Annotations[runnerIDAnnotation])
	if err != nil {
		return CredentialState{}, err
	}
	jobHandle := string(secret.Data["handle"])
	if strings.TrimSpace(jobHandle) == "" {
		return CredentialState{}, errors.New("runner credential has an empty job handle")
	}
	return CredentialState{Exists: true, RunnerID: runnerID, JobHandle: jobHandle}, nil
}

func (c *kubernetesClient) CreateCredential(ctx context.Context, slot int, registration Registration, jobHandle string) error {
	templateData, _, _, err := c.slotResources(slot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(jobHandle) == "" {
		return errors.New("job handle is empty")
	}
	secret := kubernetesSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Immutable:  true,
		Type:       "Opaque",
		Data: map[string][]byte{
			"uuid":   []byte(registration.UUID),
			"token":  []byte(registration.Token),
			"handle": []byte(jobHandle),
		},
	}
	secret.Metadata.Name = templateData.CredentialSecretName
	secret.Metadata.Namespace = templateData.Namespace
	secret.Metadata.Labels = map[string]string{"app.kubernetes.io/managed-by": "forgejo-ephemeral-runner"}
	secret.Metadata.Annotations = map[string]string{runnerIDAnnotation: fmt.Sprintf("%d", registration.ID)}
	body, err := json.Marshal(secret)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.secretsURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("create credential: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return kubernetesResponseError("create credential", resp)
	}
	return nil
}

func (c *kubernetesClient) DeleteCredential(ctx context.Context, slot int) error {
	_, _, credentialURL, err := c.slotResources(slot)
	if err != nil {
		return err
	}
	return c.delete(ctx, credentialURL, "delete credential")
}

func (c *kubernetesClient) delete(ctx context.Context, target, operation string) error {
	body := strings.NewReader(`{"apiVersion":"v1","kind":"DeleteOptions","gracePeriodSeconds":5}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNotFound:
		return nil
	default:
		return kubernetesResponseError(operation, resp)
	}
}

func (c *kubernetesClient) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultControllerUserAgent)
}

func kubernetesResponseError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s returned HTTP %d: %s", operation, resp.StatusCode, strings.TrimSpace(string(body)))
}

func parseRunnerID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("runner credential has an invalid runner ID annotation")
	}
	return id, nil
}
