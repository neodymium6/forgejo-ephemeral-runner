package controller

import (
	"context"
	"errors"
	"io"
	"log"
	"reflect"
	"testing"
)

type fakeForgejo struct {
	runners       []RemoteRunner
	registration  Registration
	listCalls     int
	registerCalls int
	deleted       []int64
	registerErr   error
	deleteErr     error
}

func (f *fakeForgejo) ListJobs(context.Context, []string) ([]RemoteJob, error) {
	return nil, nil
}

func (f *fakeForgejo) ListRunners(context.Context) ([]RemoteRunner, error) {
	f.listCalls++
	return f.runners, nil
}

func (f *fakeForgejo) RegisterRunner(context.Context, string, string) (Registration, error) {
	f.registerCalls++
	return f.registration, f.registerErr
}

func (f *fakeForgejo) DeleteRunner(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

type fakeKubernetes struct {
	pod                   PodState
	credential            CredentialState
	createPodCalls        int
	deletePodCalls        int
	createCredentialCalls int
	deleteCredentialCalls int
	createPodErr          error
	createCredentialErr   error
}

func (f *fakeKubernetes) GetPod(context.Context) (PodState, error) {
	return f.pod, nil
}

func (f *fakeKubernetes) CreatePod(context.Context) error {
	f.createPodCalls++
	return f.createPodErr
}

func (f *fakeKubernetes) DeletePod(context.Context) error {
	f.deletePodCalls++
	return nil
}

func (f *fakeKubernetes) GetCredential(context.Context) (CredentialState, error) {
	return f.credential, nil
}

func (f *fakeKubernetes) CreateCredential(context.Context, Registration) error {
	f.createCredentialCalls++
	return f.createCredentialErr
}

func (f *fakeKubernetes) DeleteCredential(context.Context) error {
	f.deleteCredentialCalls++
	return nil
}

func testConfig() Config {
	return Config{
		Namespace:            "forgejo-runners",
		RunnerName:           "ephemeral-slot-0",
		RunnerPodName:        "runner-job",
		CredentialSecretName: "runner-credential",
	}
}

func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestReconcileCreatesFreshRunner(t *testing.T) {
	registration := Registration{ID: 42, UUID: "uuid", Token: "token"}
	forgejo := &fakeForgejo{registration: registration}
	kubernetes := &fakeKubernetes{}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if forgejo.listCalls != 1 || forgejo.registerCalls != 1 {
		t.Fatalf("unexpected Forgejo calls: list=%d register=%d", forgejo.listCalls, forgejo.registerCalls)
	}
	if kubernetes.createCredentialCalls != 1 || kubernetes.createPodCalls != 1 {
		t.Fatalf("unexpected Kubernetes creates: credential=%d pod=%d", kubernetes.createCredentialCalls, kubernetes.createPodCalls)
	}
}

func TestReconcileDeletesTerminalPod(t *testing.T) {
	forgejo := &fakeForgejo{}
	kubernetes := &fakeKubernetes{
		pod:        PodState{Exists: true, Phase: "Succeeded"},
		credential: CredentialState{Exists: true, RunnerID: 42},
	}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if kubernetes.deletePodCalls != 1 {
		t.Fatalf("DeletePod calls = %d, want 1", kubernetes.deletePodCalls)
	}
	if len(forgejo.deleted) != 0 {
		t.Fatalf("Forgejo runner deleted before Pod disappeared: %v", forgejo.deleted)
	}
}

func TestReconcileCleansCredentialAfterPodDisappears(t *testing.T) {
	forgejo := &fakeForgejo{}
	kubernetes := &fakeKubernetes{credential: CredentialState{Exists: true, RunnerID: 42}}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !reflect.DeepEqual(forgejo.deleted, []int64{42}) {
		t.Fatalf("deleted runners = %v, want [42]", forgejo.deleted)
	}
	if kubernetes.deleteCredentialCalls != 1 {
		t.Fatalf("DeleteCredential calls = %d, want 1", kubernetes.deleteCredentialCalls)
	}
}

func TestReconcileRemovesManagedStaleRunner(t *testing.T) {
	forgejo := &fakeForgejo{runners: []RemoteRunner{{
		ID:          7,
		Name:        "ephemeral-slot-0",
		Description: managedDescription,
		Ephemeral:   true,
	}}}
	kubernetes := &fakeKubernetes{}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !reflect.DeepEqual(forgejo.deleted, []int64{7}) {
		t.Fatalf("deleted runners = %v, want [7]", forgejo.deleted)
	}
	if forgejo.registerCalls != 0 {
		t.Fatalf("RegisterRunner called before stale cleanup completed")
	}
}

func TestReconcileRefusesRunnerNameCollision(t *testing.T) {
	forgejo := &fakeForgejo{runners: []RemoteRunner{{
		ID:          7,
		Name:        "ephemeral-slot-0",
		Description: "created elsewhere",
		Ephemeral:   true,
	}}}

	err := Reconcile(context.Background(), testConfig(), forgejo, &fakeKubernetes{}, testLogger())
	if err == nil {
		t.Fatal("Reconcile() succeeded with unmanaged runner name collision")
	}
	if len(forgejo.deleted) != 0 {
		t.Fatalf("deleted unmanaged runner: %v", forgejo.deleted)
	}
}

func TestReconcileCleansRegistrationAfterCredentialFailure(t *testing.T) {
	registration := Registration{ID: 42, UUID: "uuid", Token: "token"}
	forgejo := &fakeForgejo{registration: registration}
	kubernetes := &fakeKubernetes{createCredentialErr: errors.New("injected failure")}

	err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger())
	if err == nil {
		t.Fatal("Reconcile() succeeded after credential creation failure")
	}
	if !reflect.DeepEqual(forgejo.deleted, []int64{42}) {
		t.Fatalf("deleted runners = %v, want [42]", forgejo.deleted)
	}
}

func TestScopeAPIPath(t *testing.T) {
	tests := map[string]string{
		"user":                       "/api/v1/user/actions/runners",
		"instance":                   "/api/v1/admin/actions/runners",
		"organization:example":       "/api/v1/orgs/example/actions/runners",
		"repository:example/project": "/api/v1/repos/example/project/actions/runners",
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := scopeAPIPath(input)
			if err != nil {
				t.Fatalf("scopeAPIPath() error = %v", err)
			}
			if got != want {
				t.Fatalf("scopeAPIPath() = %q, want %q", got, want)
			}
		})
	}
}

func TestScopeAPIPathRejectsMalformedScope(t *testing.T) {
	for _, input := range []string{"", "repository:owner", "organization:a/b", "unknown:value"} {
		t.Run(input, func(t *testing.T) {
			if _, err := scopeAPIPath(input); err == nil {
				t.Fatalf("scopeAPIPath(%q) succeeded", input)
			}
		})
	}
}
