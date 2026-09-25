package controller

import (
	"context"
	"errors"
	"testing"
)

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
