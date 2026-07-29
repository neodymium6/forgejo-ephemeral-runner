package controller

import (
	"context"
	"errors"
	"io"
	"log"
	"reflect"
	"testing"
	"time"
)

const testScope = "example/project"

type fakeForgejo struct {
	jobs             []RemoteJob
	runners          []RemoteRunner
	registration     Registration
	listCalls        int
	registerCalls    int
	registeredNames  []string
	registeredScopes []string
	deleted          []int64
	deletedScopes    []string
	registerErr      error
	deleteErr        error
}

func (f *fakeForgejo) ListJobs(context.Context, []string) ([]RemoteJob, error) {
	jobs := append([]RemoteJob(nil), f.jobs...)
	for index := range jobs {
		if jobs[index].Scope == "" {
			jobs[index].Scope = testScope
		}
	}
	return jobs, nil
}

func (f *fakeForgejo) ListRunners(context.Context) ([]RemoteRunner, error) {
	f.listCalls++
	runners := append([]RemoteRunner(nil), f.runners...)
	for index := range runners {
		if runners[index].Scope == "" {
			runners[index].Scope = testScope
		}
	}
	return runners, nil
}

func (f *fakeForgejo) RegisterRunner(_ context.Context, scope, name, _ string) (Registration, error) {
	f.registerCalls++
	f.registeredScopes = append(f.registeredScopes, scope)
	f.registeredNames = append(f.registeredNames, name)
	return f.registration, f.registerErr
}

func (f *fakeForgejo) DeleteRunner(_ context.Context, scope string, id int64) error {
	f.deletedScopes = append(f.deletedScopes, scope)
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

type fakeKubernetes struct {
	pod                   PodState
	credential            CredentialState
	createPodCalls        int
	deletePodCalls        int
	deletePodUIDs         []string
	createCredentialCalls int
	deleteCredentialCalls int
	deleteCredentialUIDs  []string
	createPodErr          error
	createCredentialErr   error
	credentialHandles     []string
	credentialScopes      []string
}

func (f *fakeKubernetes) GetPod(context.Context, int) (PodState, error) {
	return f.pod, nil
}

func (f *fakeKubernetes) CreatePod(context.Context, int) error {
	f.createPodCalls++
	return f.createPodErr
}

func (f *fakeKubernetes) DeletePod(_ context.Context, _ int, uid string) error {
	f.deletePodCalls++
	f.deletePodUIDs = append(f.deletePodUIDs, uid)
	return nil
}

func (f *fakeKubernetes) GetCredential(context.Context, int) (CredentialState, error) {
	credential := f.credential
	if credential.Exists && credential.Scope == "" {
		credential.Scope = testScope
	}
	return credential, nil
}

func (f *fakeKubernetes) CreateCredential(_ context.Context, _ int, _ Registration, scope, handle string) (string, error) {
	f.createCredentialCalls++
	f.credentialScopes = append(f.credentialScopes, scope)
	f.credentialHandles = append(f.credentialHandles, handle)
	if f.createCredentialErr != nil {
		return "", f.createCredentialErr
	}
	return "created-credential-uid", nil
}

func (f *fakeKubernetes) DeleteCredential(_ context.Context, _ int, uid string) error {
	f.deleteCredentialCalls++
	f.deleteCredentialUIDs = append(f.deleteCredentialUIDs, uid)
	return nil
}

func testConfig() Config {
	return Config{
		ForgejoRepositoryAllowlist: []string{testScope},
		Namespace:                  "forgejo-runners",
		RunnerName:                 "ephemeral-slot-0",
		RunnerLabels:               []string{"linux-amd64"},
		RunnerPodName:              "runner-job",
		CredentialSecretName:       "runner-credential",
		RunnerStartupTimeout:       30 * time.Minute,
		RunnerUnknownTimeout:       5 * time.Minute,
		MaxConcurrent:              1,
	}
}

func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestReconcileCreatesFreshRunner(t *testing.T) {
	registration := Registration{ID: 42, UUID: "uuid", Token: "token"}
	forgejo := &fakeForgejo{
		jobs:         []RemoteJob{{ID: 11, Attempt: 0, Handle: "job-handle", Status: "waiting"}},
		registration: registration,
	}
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
		pod:        PodState{Exists: true, UID: "pod-uid", Phase: "Succeeded"},
		credential: CredentialState{Exists: true, UID: "credential-uid", RunnerID: 42},
	}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if kubernetes.deletePodCalls != 1 {
		t.Fatalf("DeletePod calls = %d, want 1", kubernetes.deletePodCalls)
	}
	if !reflect.DeepEqual(kubernetes.deletePodUIDs, []string{"pod-uid"}) {
		t.Fatalf("deleted Pod UIDs = %v", kubernetes.deletePodUIDs)
	}
	if len(forgejo.deleted) != 0 {
		t.Fatalf("Forgejo runner deleted before Pod disappeared: %v", forgejo.deleted)
	}
}

func TestReconcileRecoversStalledPods(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		phase      string
		createdAt  time.Time
		wantDelete bool
	}{
		{name: "expired Pending", phase: "Pending", createdAt: now.Add(-31 * time.Minute), wantDelete: true},
		{name: "recent Pending", phase: "Pending", createdAt: now.Add(-29 * time.Minute)},
		{name: "expired Unknown", phase: "Unknown", createdAt: now.Add(-6 * time.Minute), wantDelete: true},
		{name: "recent Unknown", phase: "Unknown", createdAt: now.Add(-4 * time.Minute)},
		{name: "old Running", phase: "Running", createdAt: now.Add(-24 * time.Hour)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forgejo := &fakeForgejo{}
			kubernetes := &fakeKubernetes{
				pod:        PodState{Exists: true, UID: "pod-uid", Phase: test.phase, CreatedAt: test.createdAt},
				credential: CredentialState{Exists: true, UID: "credential-uid", RunnerID: 42, JobHandle: "job-handle"},
			}

			if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			wantCalls := 0
			if test.wantDelete {
				wantCalls = 1
			}
			if kubernetes.deletePodCalls != wantCalls {
				t.Fatalf("DeletePod calls = %d, want %d", kubernetes.deletePodCalls, wantCalls)
			}
		})
	}
}

func TestReconcileCleansCredentialAfterPodDisappears(t *testing.T) {
	forgejo := &fakeForgejo{runners: []RemoteRunner{{
		ID:          42,
		Name:        "ephemeral-slot-0-0",
		Description: managedDescription,
		Ephemeral:   true,
	}}}
	kubernetes := &fakeKubernetes{credential: CredentialState{Exists: true, UID: "credential-uid", RunnerID: 42}}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !reflect.DeepEqual(forgejo.deleted, []int64{42}) {
		t.Fatalf("deleted runners = %v, want [42]", forgejo.deleted)
	}
	if kubernetes.deleteCredentialCalls != 1 {
		t.Fatalf("DeleteCredential calls = %d, want 1", kubernetes.deleteCredentialCalls)
	}
	if !reflect.DeepEqual(kubernetes.deleteCredentialUIDs, []string{"credential-uid"}) {
		t.Fatalf("deleted credential UIDs = %v", kubernetes.deleteCredentialUIDs)
	}
}

func TestReconcileRefusesCredentialPointingToUnmanagedRunner(t *testing.T) {
	forgejo := &fakeForgejo{runners: []RemoteRunner{{
		ID:          42,
		Name:        "another-runner",
		Description: managedDescription,
		Ephemeral:   true,
	}}}
	kubernetes := &fakeKubernetes{credential: CredentialState{
		Exists: true, UID: "credential-uid", RunnerID: 42,
	}}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err == nil {
		t.Fatal("Reconcile() accepted a credential pointing to an unmanaged runner")
	}
	if len(forgejo.deleted) != 0 || kubernetes.deleteCredentialCalls != 0 {
		t.Fatalf("unsafe cleanup occurred: runners=%v credentials=%d", forgejo.deleted, kubernetes.deleteCredentialCalls)
	}
}

func TestReconcileDeletesCredentialWhenManagedRunnerIsAlreadyAbsent(t *testing.T) {
	forgejo := &fakeForgejo{}
	kubernetes := &fakeKubernetes{credential: CredentialState{
		Exists: true, UID: "credential-uid", RunnerID: 42,
	}}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(forgejo.deleted) != 0 {
		t.Fatalf("DeleteRunner called for an absent runner: %v", forgejo.deleted)
	}
	if !reflect.DeepEqual(kubernetes.deleteCredentialUIDs, []string{"credential-uid"}) {
		t.Fatalf("deleted credential UIDs = %v", kubernetes.deleteCredentialUIDs)
	}
}

func TestReconcileRemovesManagedStaleRunner(t *testing.T) {
	forgejo := &fakeForgejo{runners: []RemoteRunner{{
		ID:          7,
		Name:        "ephemeral-slot-0-0",
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
	forgejo := &fakeForgejo{jobs: []RemoteJob{{ID: 11, Attempt: 1, Handle: "job-handle", Status: "waiting"}},
		runners: []RemoteRunner{{
			ID:          7,
			Name:        "ephemeral-slot-0-0",
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
	forgejo := &fakeForgejo{
		jobs:         []RemoteJob{{ID: 11, Attempt: 1, Handle: "job-handle", Status: "waiting"}},
		registration: registration,
	}
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
	for _, input := range []string{
		"",
		"organization:.",
		"organization:..",
		"organization:a/b",
		"repository:owner",
		"repository:./admin",
		"repository:../admin",
		"repository:owner/.",
		"repository:owner/..",
		"unknown:value",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := scopeAPIPath(input); err == nil {
				t.Fatalf("scopeAPIPath(%q) succeeded", input)
			}
		})
	}
}

func TestReconcileScalesToZeroWithoutWaitingJobs(t *testing.T) {
	forgejo := &fakeForgejo{}
	kubernetes := &fakeKubernetes{}

	if err := Reconcile(context.Background(), testConfig(), forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if forgejo.registerCalls != 0 {
		t.Fatalf("RegisterRunner calls = %d, want 0", forgejo.registerCalls)
	}
	if kubernetes.createPodCalls != 0 || kubernetes.createCredentialCalls != 0 {
		t.Fatalf("unexpected Kubernetes creates: pod=%d credential=%d", kubernetes.createPodCalls, kubernetes.createCredentialCalls)
	}
}

func TestReconcileHonorsMaxConcurrentAndTargetsWaitingJobs(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConcurrent = 2
	registration := Registration{ID: 42, UUID: "uuid", Token: "token"}
	forgejo := &fakeForgejo{
		jobs: []RemoteJob{
			{ID: 11, Attempt: 1, Handle: "first-handle", Status: "waiting"},
			{ID: 12, Attempt: 1, Handle: "second-handle", Status: "waiting"},
			{ID: 13, Attempt: 1, Handle: "running-handle", Status: "running"},
		},
		registration: registration,
	}
	kubernetes := &fakeKubernetes{}

	if err := Reconcile(context.Background(), cfg, forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if forgejo.registerCalls != 2 {
		t.Fatalf("RegisterRunner calls = %d, want 2", forgejo.registerCalls)
	}
	wantNames := []string{"ephemeral-slot-0-0", "ephemeral-slot-0-1"}
	if !reflect.DeepEqual(forgejo.registeredNames, wantNames) {
		t.Fatalf("registered names = %v, want %v", forgejo.registeredNames, wantNames)
	}
	if !reflect.DeepEqual(kubernetes.credentialHandles, []string{"first-handle", "second-handle"}) {
		t.Fatalf("credential handles = %v", kubernetes.credentialHandles)
	}
}

func TestReconcileTargetsSameHandleInDifferentRepositoryScopes(t *testing.T) {
	cfg := testConfig()
	cfg.ForgejoRepositoryAllowlist = []string{"example/one", "example/two"}
	cfg.MaxConcurrent = 2
	forgejo := &fakeForgejo{
		jobs: []RemoteJob{
			{Scope: "example/one", ID: 11, Attempt: 1, Handle: "shared-handle", Status: "waiting"},
			{Scope: "example/two", ID: 12, Attempt: 1, Handle: "shared-handle", Status: "waiting"},
		},
		registration: Registration{ID: 42, UUID: "uuid", Token: "token"},
	}
	kubernetes := &fakeKubernetes{}

	if err := Reconcile(context.Background(), cfg, forgejo, kubernetes, testLogger()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantScopes := []string{"example/one", "example/two"}
	if !reflect.DeepEqual(forgejo.registeredScopes, wantScopes) {
		t.Fatalf("registered scopes = %v, want %v", forgejo.registeredScopes, wantScopes)
	}
	if !reflect.DeepEqual(kubernetes.credentialScopes, wantScopes) {
		t.Fatalf("credential scopes = %v, want %v", kubernetes.credentialScopes, wantScopes)
	}
	if kubernetes.createPodCalls != 2 {
		t.Fatalf("CreatePod calls = %d, want 2", kubernetes.createPodCalls)
	}
}

func TestReconcileRetainsCredentialOutsideCurrentAllowlist(t *testing.T) {
	cfg := testConfig()
	forgejo := &fakeForgejo{}
	kubernetes := &fakeKubernetes{credential: CredentialState{Exists: true, UID: "credential-uid", Scope: "example/removed", RunnerID: 42, JobHandle: "job-handle"}}

	err := Reconcile(context.Background(), cfg, forgejo, kubernetes, testLogger())
	if err == nil {
		t.Fatal("Reconcile() accepted a credential outside the current allowlist")
	}
	if len(forgejo.deleted) != 0 {
		t.Fatalf("remote runner cleanup occurred: %v", forgejo.deleted)
	}
	if kubernetes.deleteCredentialCalls != 0 {
		t.Fatalf("credential cleanup occurred: %d", kubernetes.deleteCredentialCalls)
	}
}
