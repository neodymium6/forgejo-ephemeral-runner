package controller

import (
	"context"
	"errors"
	"testing"
)

func TestFullCapacityObservesQueueWithoutDeletingReservations(t *testing.T) {
	for _, test := range []struct {
		name      string
		jobs      []RemoteJob
		err       error
		unmatched int64
	}{
		{name: "cancelled reservation", jobs: []RemoteJob{{ID: 12, Handle: "next", Status: "waiting"}}, unmatched: 1},
		{name: "assigned task", jobs: []RemoteJob{{ID: 11, Handle: "reserved", Status: "running"}, {ID: 12, Handle: "next", Status: "waiting"}}},
		{name: "incomplete response", unmatched: 1},
		{name: "temporary API failure", err: errors.New("unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			forgejo := &fakeForgejo{jobs: test.jobs, jobsErr: test.err}
			kubernetes := &fakeKubernetes{
				pod:        PodState{Exists: true, UID: "pod-uid", Phase: "Running"},
				credential: CredentialState{Exists: true, UID: "secret-uid", RunnerID: 42, JobHandle: "reserved"},
			}
			// Fresh process-local metrics on each pass model restart / a new
			// Lease holder: observation must not authorize destructive cleanup.
			for range 2 {
				metrics := NewMetrics("dev")
				err := reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger(), metrics)
				if !errors.Is(err, test.err) {
					t.Fatalf("reconcile error = %v, want %v", err, test.err)
				}
				if got := metrics.unmatchedReservations.Load(); got != test.unmatched {
					t.Fatalf("unmatched reservations = %d, want %d", got, test.unmatched)
				}
				wantObserved := int64(1)
				if test.err != nil {
					wantObserved = 0
				}
				if got := metrics.queueObserved.Load(); got != wantObserved {
					t.Fatalf("queue observed = %d, want %d", got, wantObserved)
				}
				if len(test.jobs) > 0 && metrics.waitingJobs.Load() != 1 {
					t.Fatal("full capacity concealed the next waiting job")
				}
			}
			if forgejo.jobsCalls != 2 || forgejo.registerCalls != 0 || len(forgejo.deleted) != 0 ||
				kubernetes.deletePodCalls != 0 || kubernetes.deleteCredentialCalls != 0 {
				t.Fatal("full capacity must observe jobs without mutating an active reservation")
			}
		})
	}
}

func TestUnmatchedObservationClearsAfterAPIFailure(t *testing.T) {
	forgejo := &fakeForgejo{}
	kubernetes := &fakeKubernetes{
		pod:        PodState{Exists: true, UID: "pod-uid", Phase: "Running"},
		credential: CredentialState{Exists: true, UID: "secret-uid", RunnerID: 42, JobHandle: "reserved"},
	}
	metrics := NewMetrics("dev")
	if err := reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger(), metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.unmatchedReservations.Load() != 1 {
		t.Fatal("missing reservation was not observed")
	}
	forgejo.jobsErr = errors.New("unavailable")
	if err := reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger(), metrics); err == nil {
		t.Fatal("expected API error")
	}
	if metrics.unmatchedReservations.Load() != 0 || metrics.queueObserved.Load() != 0 {
		t.Fatal("failed observation retained stale queue state")
	}
}

func TestIdleRunnerExitReleasesSlotInOrder(t *testing.T) {
	cfg := testConfig()
	forgejo := &fakeForgejo{
		jobs:         []RemoteJob{{ID: 12, Handle: "next", Status: "waiting"}},
		runners:      []RemoteRunner{{ID: 42, Name: runnerNameForSlot(cfg, 0), Description: managedDescription, Ephemeral: true}},
		registration: Registration{ID: 43, UUID: "next-uuid", Token: "fake-next-token"},
	}
	kubernetes := &fakeKubernetes{
		pod:        PodState{Exists: true, UID: "old-pod-uid", Phase: "Failed"},
		credential: CredentialState{Exists: true, UID: "old-secret-uid", RunnerID: 42, JobHandle: "cancelled"},
	}
	reconcileOnce := func() error {
		return Reconcile(context.Background(), cfg, forgejo, kubernetes, testLogger())
	}
	kubernetes.getPodErr = errors.New("API unavailable")
	if err := reconcileOnce(); err == nil || kubernetes.deletePodCalls != 0 || len(forgejo.deleted) != 0 {
		t.Fatal("failed Pod observation must not trigger cleanup")
	}
	kubernetes.getPodErr = nil
	kubernetes.deletePodErr = errors.New("UID conflict")
	if err := reconcileOnce(); err == nil || len(forgejo.deleted) != 0 || kubernetes.deleteCredentialCalls != 0 {
		t.Fatal("failed Pod deletion must preserve registration and credential")
	}
	kubernetes.deletePodErr = nil
	if err := reconcileOnce(); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.deleted) != 0 || kubernetes.deletePodUIDs[1] != "old-pod-uid" {
		t.Fatal("first cleanup must delete only the original Pod with its UID")
	}
	kubernetes.pod = PodState{}
	forgejo.deleteErr = errors.New("temporary Forgejo error")
	if err := reconcileOnce(); err == nil || kubernetes.deleteCredentialCalls != 0 {
		t.Fatal("failed remote cleanup must preserve the credential for retry")
	}
	forgejo.deleteErr = nil
	if err := reconcileOnce(); err != nil {
		t.Fatal(err)
	}
	if kubernetes.deleteCredentialCalls != 1 || kubernetes.deleteCredentialUIDs[0] != "old-secret-uid" || forgejo.registerCalls != 0 {
		t.Fatal("credential cleanup must finish before reusing the slot")
	}
	forgejo.runners = nil
	kubernetes.credential = CredentialState{}
	if err := reconcileOnce(); err != nil {
		t.Fatal(err)
	}
	if forgejo.registerCalls != 1 || kubernetes.createPodCalls != 1 || kubernetes.credentialHandles[0] != "next" {
		t.Fatal("released slot did not start the next job")
	}
}
