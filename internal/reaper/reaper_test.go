package reaper

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeClient struct {
	mu      sync.Mutex
	pod     podStatus
	deleted int
}

func (f *fakeClient) GetPod(context.Context) (podStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pod, nil
}

func (f *fakeClient) DeletePod(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted++
	return nil
}

func (f *fakeClient) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted
}

func TestRunDeletesWhenCompletionMarkerExists(t *testing.T) {
	t.Parallel()

	completionFile := filepath.Join(t.TempDir(), "completed")
	if err := os.WriteFile(completionFile, []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{}
	cfg := Config{
		Namespace:      "runner-system",
		PodName:        "runner-abc",
		CompletionFile: completionFile,
		PollInterval:   time.Millisecond,
	}

	if err := Run(context.Background(), cfg, client, log.New(os.Stderr, "", 0)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := client.deleteCount(); got != 1 {
		t.Fatalf("DeletePod() calls = %d, want 1", got)
	}
}

func TestDeletionReasonDetectsRunnerRestart(t *testing.T) {
	t.Parallel()

	client := &fakeClient{}
	client.pod.Status.ContainerStatuses = []containerStatus{{
		Name:         "runner",
		RestartCount: 1,
	}}

	cfg := Config{
		RunnerContainer: "runner",
		CompletionFile:  filepath.Join(t.TempDir(), "missing"),
	}

	reason, err := deletionReason(cfg, client, context.Background())
	if err != nil {
		t.Fatalf("deletionReason() error = %v", err)
	}
	if reason == "" {
		t.Fatal("deletionReason() returned an empty reason")
	}
}

func TestDeletionReasonKeepsHealthyRunner(t *testing.T) {
	t.Parallel()

	client := &fakeClient{}
	client.pod.Status.ContainerStatuses = []containerStatus{{Name: "runner"}}

	cfg := Config{
		RunnerContainer: "runner",
		CompletionFile:  filepath.Join(t.TempDir(), "missing"),
	}

	reason, err := deletionReason(cfg, client, context.Background())
	if err != nil {
		t.Fatalf("deletionReason() error = %v", err)
	}
	if reason != "" {
		t.Fatalf("deletionReason() = %q, want empty", reason)
	}
}

func TestConfigRejectsShortPollInterval(t *testing.T) {
	t.Setenv("KUBERNETES_API_URL", "https://kubernetes.default.svc")
	t.Setenv("POD_NAMESPACE", "runner-system")
	t.Setenv("POD_NAME", "runner-abc")
	t.Setenv("POLL_INTERVAL", "10ms")

	if _, err := ConfigFromEnvironment(); err == nil {
		t.Fatal("ConfigFromEnvironment() returned nil error")
	}
}

func TestKubernetesClientGetsAndDeletesOnlyConfiguredPod(t *testing.T) {
	t.Parallel()

	const token = "test-service-account-token"
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.URL.Path; got != "/api/v1/namespaces/runner-system/pods/runner-abc" {
			t.Errorf("path = %q", got)
		}
		requests <- r.Method

		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{},
				"status": map[string]any{
					"containerStatuses": []map[string]any{{
						"name":         "runner",
						"restartCount": 0,
						"state":        map[string]any{"running": map[string]any{}},
					}},
				},
			})
		case http.MethodDelete:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read delete body: %v", err)
			}
			if string(body) != `{"apiVersion":"v1","kind":"DeleteOptions","gracePeriodSeconds":5}` {
				t.Errorf("delete body = %q", body)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		APIURL:    server.URL,
		Namespace: "runner-system",
		PodName:   "runner-abc",
		TokenPath: tokenFile,
	}
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.GetPod(context.Background()); err != nil {
		t.Fatalf("GetPod() error = %v", err)
	}
	if err := client.DeletePod(context.Background()); err != nil {
		t.Fatalf("DeletePod() error = %v", err)
	}

	if got := <-requests; got != http.MethodGet {
		t.Fatalf("first request = %s", got)
	}
	if got := <-requests; got != http.MethodDelete {
		t.Fatalf("second request = %s", got)
	}
}
