package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsHandlerExposesFixedSchema(t *testing.T) {
	metrics := NewMetrics("0.4.0\n\"test\\build")
	metrics.setLeader(true)
	metrics.setRunnerSlots(2, 4)
	metrics.setQueue(3, true)
	metrics.setUnmatchedReservations(1)
	metrics.observeReconcile(125*time.Millisecond, nil)
	metrics.observeReconcile(375*time.Millisecond, errors.New("failed"))
	metrics.observeOperation(operationPodCreate, nil)
	metrics.observeOperation(operationPodCreate, errors.New("failed"))
	metrics.observeCleanup(cleanupPendingTimeout)

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != metricsContentType {
		t.Fatalf("Content-Type = %q, want %q", got, metricsContentType)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	body := response.Body.String()
	for _, want := range []string{
		`forgejo_ephemeral_runner_build_info{version="0.4.0\n\"test\\build"} 1`,
		"forgejo_ephemeral_runner_leader 1",
		"forgejo_ephemeral_runner_runner_slots_active 2",
		"forgejo_ephemeral_runner_runner_slots_capacity 4",
		"forgejo_ephemeral_runner_waiting_jobs 3",
		"forgejo_ephemeral_runner_queue_observed 1",
		"forgejo_ephemeral_runner_reservations_unmatched 1",
		`forgejo_ephemeral_runner_reconciliations_total{result="success"} 1`,
		`forgejo_ephemeral_runner_reconciliations_total{result="failure"} 1`,
		"forgejo_ephemeral_runner_reconcile_duration_seconds_sum 0.5",
		"forgejo_ephemeral_runner_reconcile_duration_seconds_count 2",
		`forgejo_ephemeral_runner_operations_total{operation="pod_create",result="success"} 1`,
		`forgejo_ephemeral_runner_operations_total{operation="pod_create",result="failure"} 1`,
		`forgejo_ephemeral_runner_cleanup_attempts_total{reason="pending_timeout"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body does not contain %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"repository=", "job=", "runner=", "token="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("metrics body contains forbidden dynamic label %q", forbidden)
		}
	}
}

func TestMetricsHandlerRejectsOtherMethodsAndPaths(t *testing.T) {
	metrics := NewMetrics("dev")

	post := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST /metrics status = %d, Allow = %q", post.Code, post.Header().Get("Allow"))
	}

	root := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusNotFound {
		t.Fatalf("GET / status = %d, want %d", root.Code, http.StatusNotFound)
	}
}

func TestReconcileUpdatesMetrics(t *testing.T) {
	forgejo := &fakeForgejo{
		jobs:         []RemoteJob{{ID: 11, Handle: "opaque-handle", Status: "waiting"}},
		registration: Registration{ID: 42, UUID: "uuid", Token: "one-job-token"},
	}
	kubernetes := &fakeKubernetes{}
	metrics := NewMetrics("dev")

	err := reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger(), metrics)
	if err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	if got := metrics.activeSlots.Load(); got != 1 {
		t.Fatalf("active slots = %d, want 1", got)
	}
	if got := metrics.slotCapacity.Load(); got != 1 {
		t.Fatalf("slot capacity = %d, want 1", got)
	}
	if got := metrics.waitingJobs.Load(); got != 1 {
		t.Fatalf("waiting jobs = %d, want 1", got)
	}
	if got := metrics.queueObserved.Load(); got != 1 {
		t.Fatalf("queue observed = %d, want 1", got)
	}
	for _, operation := range []string{operationRegistrationCreate, operationCredentialCreate, operationPodCreate} {
		index := operationMetricIndex(operation)
		if got := metrics.operations[index].success.Load(); got != 1 {
			t.Errorf("%s successes = %d, want 1", operation, got)
		}
	}
}

func TestLeaderMetricClearsReplicaState(t *testing.T) {
	metrics := NewMetrics("dev")
	metrics.setLeader(true)
	metrics.setRunnerSlots(2, 4)
	metrics.setQueue(3, true)
	metrics.setUnmatchedReservations(1)

	metrics.setLeader(false)

	if metrics.leader.Load() != 0 || metrics.activeSlots.Load() != 0 ||
		metrics.waitingJobs.Load() != 0 || metrics.queueObserved.Load() != 0 || metrics.unmatchedReservations.Load() != 0 {
		t.Fatal("follower metrics retained leader-only state")
	}
	if metrics.slotCapacity.Load() != 4 {
		t.Fatalf("slot capacity = %d, want 4", metrics.slotCapacity.Load())
	}
}
