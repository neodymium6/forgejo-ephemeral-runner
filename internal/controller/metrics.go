package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

const (
	operationRegistrationCreate = "registration_create"
	operationRegistrationDelete = "registration_delete"
	operationCredentialCreate   = "credential_create"
	operationCredentialDelete   = "credential_delete"
	operationPodCreate          = "pod_create"
	operationPodDelete          = "pod_delete"
)

const (
	cleanupOrphanPod         = "orphan_pod"
	cleanupPendingTimeout    = "pending_timeout"
	cleanupUnknownTimeout    = "unknown_timeout"
	cleanupPodSucceeded      = "pod_succeeded"
	cleanupPodFailed         = "pod_failed"
	cleanupStaleRegistration = "stale_registration"
)

var metricOperations = [...]string{
	operationRegistrationCreate,
	operationRegistrationDelete,
	operationCredentialCreate,
	operationCredentialDelete,
	operationPodCreate,
	operationPodDelete,
}

var metricCleanupReasons = [...]string{
	cleanupOrphanPod,
	cleanupPendingTimeout,
	cleanupUnknownTimeout,
	cleanupPodSucceeded,
	cleanupPodFailed,
	cleanupStaleRegistration,
}

type operationCounters struct {
	success atomic.Uint64
	failure atomic.Uint64
}

// Metrics stores a fixed, low-cardinality set of Prometheus metrics.
type Metrics struct {
	version string

	leader                atomic.Int64
	activeSlots           atomic.Int64
	slotCapacity          atomic.Int64
	waitingJobs           atomic.Int64
	queueObserved         atomic.Int64
	unmatchedReservations atomic.Int64
	reconcileSuccess      atomic.Uint64
	reconcileFailure      atomic.Uint64
	reconcileDurationNano atomic.Uint64
	operations            [len(metricOperations)]operationCounters
	cleanup               [len(metricCleanupReasons)]atomic.Uint64
}

func NewMetrics(version string) *Metrics {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	return &Metrics{version: version}
}

func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.serveHTTP)
	return mux
}

func (m *Metrics) setLeader(leader bool) {
	if m == nil {
		return
	}
	if leader {
		m.leader.Store(1)
		return
	}
	m.leader.Store(0)
	m.activeSlots.Store(0)
	m.waitingJobs.Store(0)
	m.queueObserved.Store(0)
	m.unmatchedReservations.Store(0)
}

func (m *Metrics) setUnmatchedReservations(count int) {
	if m != nil {
		m.unmatchedReservations.Store(int64(count))
	}
}

func (m *Metrics) setRunnerSlots(active, capacity int) {
	if m == nil {
		return
	}
	m.activeSlots.Store(int64(active))
	m.slotCapacity.Store(int64(capacity))
}

func (m *Metrics) setQueue(waiting int, observed bool) {
	if m == nil {
		return
	}
	if !observed {
		m.waitingJobs.Store(0)
		m.queueObserved.Store(0)
		return
	}
	m.waitingJobs.Store(int64(waiting))
	m.queueObserved.Store(1)
}

func (m *Metrics) observeReconcile(duration time.Duration, err error) {
	if m == nil {
		return
	}
	if err == nil {
		m.reconcileSuccess.Add(1)
	} else {
		m.reconcileFailure.Add(1)
	}
	if duration > 0 {
		m.reconcileDurationNano.Add(uint64(duration))
	}
}

func (m *Metrics) observeOperation(operation string, err error) {
	if m == nil {
		return
	}
	index := operationMetricIndex(operation)
	if index < 0 {
		return
	}
	if err == nil {
		m.operations[index].success.Add(1)
	} else {
		m.operations[index].failure.Add(1)
	}
}

func (m *Metrics) observeCleanup(reason string) {
	if m == nil {
		return
	}
	index := cleanupMetricIndex(reason)
	if index >= 0 {
		m.cleanup[index].Add(1)
	}
}

func operationMetricIndex(operation string) int {
	for index, candidate := range metricOperations {
		if operation == candidate {
			return index
		}
	}
	return -1
}

func cleanupMetricIndex(reason string) int {
	for index, candidate := range metricCleanupReasons {
		if reason == candidate {
			return index
		}
	}
	return -1
}

func (m *Metrics) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body bytes.Buffer
	writeMetricHeader(&body, "forgejo_ephemeral_runner_build_info", "Static build information.", "gauge")
	fmt.Fprintf(
		&body,
		"forgejo_ephemeral_runner_build_info{version=\"%s\"} 1\n",
		escapeMetricLabel(m.version),
	)
	writeMetricHeader(&body, "forgejo_ephemeral_runner_leader", "Whether this controller replica currently holds the leader Lease.", "gauge")
	fmt.Fprintf(&body, "forgejo_ephemeral_runner_leader %d\n", m.leader.Load())
	writeMetricHeader(&body, "forgejo_ephemeral_runner_runner_slots_active", "Runner slots currently backed by an active or deleting Pod.", "gauge")
	fmt.Fprintf(&body, "forgejo_ephemeral_runner_runner_slots_active %d\n", m.activeSlots.Load())
	writeMetricHeader(&body, "forgejo_ephemeral_runner_runner_slots_capacity", "Configured maximum runner slot count.", "gauge")
	fmt.Fprintf(&body, "forgejo_ephemeral_runner_runner_slots_capacity %d\n", m.slotCapacity.Load())
	writeMetricHeader(&body, "forgejo_ephemeral_runner_waiting_jobs", "Matching waiting jobs seen during the most recent queue observation.", "gauge")
	fmt.Fprintf(&body, "forgejo_ephemeral_runner_waiting_jobs %d\n", m.waitingJobs.Load())
	writeMetricHeader(&body, "forgejo_ephemeral_runner_queue_observed", "Whether the current leader observed the queue during its latest reconciliation.", "gauge")
	fmt.Fprintf(&body, "forgejo_ephemeral_runner_queue_observed %d\n", m.queueObserved.Load())
	writeMetricHeader(&body, "forgejo_ephemeral_runner_reservations_unmatched", "Active reservations absent from the latest observed job list; not proof of cancellation.", "gauge")
	fmt.Fprintf(&body, "forgejo_ephemeral_runner_reservations_unmatched %d\n", m.unmatchedReservations.Load())
	writeMetricHeader(&body, "forgejo_ephemeral_runner_reconciliations_total", "Completed reconciliation attempts.", "counter")
	fmt.Fprintf(&body, "forgejo_ephemeral_runner_reconciliations_total{result=\"success\"} %d\n", m.reconcileSuccess.Load())
	fmt.Fprintf(&body, "forgejo_ephemeral_runner_reconciliations_total{result=\"failure\"} %d\n", m.reconcileFailure.Load())
	writeMetricHeader(&body, "forgejo_ephemeral_runner_reconcile_duration_seconds", "Time spent in reconciliation.", "summary")
	fmt.Fprintf(
		&body,
		"forgejo_ephemeral_runner_reconcile_duration_seconds_sum %s\n",
		strconv.FormatFloat(float64(m.reconcileDurationNano.Load())/float64(time.Second), 'g', -1, 64),
	)
	fmt.Fprintf(
		&body,
		"forgejo_ephemeral_runner_reconcile_duration_seconds_count %d\n",
		m.reconcileSuccess.Load()+m.reconcileFailure.Load(),
	)
	writeMetricHeader(&body, "forgejo_ephemeral_runner_operations_total", "Mutating Forgejo and Kubernetes operations.", "counter")
	for index, operation := range metricOperations {
		fmt.Fprintf(
			&body,
			"forgejo_ephemeral_runner_operations_total{operation=%q,result=\"success\"} %d\n",
			operation,
			m.operations[index].success.Load(),
		)
		fmt.Fprintf(
			&body,
			"forgejo_ephemeral_runner_operations_total{operation=%q,result=\"failure\"} %d\n",
			operation,
			m.operations[index].failure.Load(),
		)
	}
	writeMetricHeader(&body, "forgejo_ephemeral_runner_cleanup_attempts_total", "Cleanup decisions by fixed, non-sensitive reason.", "counter")
	for index, reason := range metricCleanupReasons {
		fmt.Fprintf(
			&body,
			"forgejo_ephemeral_runner_cleanup_attempts_total{reason=%q} %d\n",
			reason,
			m.cleanup[index].Load(),
		)
	}

	writer.Header().Set("Content-Type", metricsContentType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body.Bytes())
}

func writeMetricHeader(body *bytes.Buffer, name, help, metricType string) {
	fmt.Fprintf(body, "# HELP %s %s\n", name, help)
	fmt.Fprintf(body, "# TYPE %s %s\n", name, metricType)
}

func escapeMetricLabel(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")
	return replacer.Replace(value)
}
