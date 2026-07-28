package controller

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testLeaderElector(server *httptest.Server, now time.Time) *LeaderElector {
	return &LeaderElector{
		httpClient:    server.Client(),
		leasesURL:     server.URL + "/leases",
		leaseURL:      server.URL + "/leases/controller",
		identity:      "controller-0",
		namespace:     "forgejo-runners",
		leaseName:     "controller",
		leaseDuration: defaultLeaseDuration,
		renewDeadline: defaultRenewDeadline,
		retryPeriod:   defaultRetryPeriod,
		now:           func() time.Time { return now },
		logger:        log.New(io.Discard, "", 0),
	}
}

func writeLease(t *testing.T, w http.ResponseWriter, lease kubernetesLease, status int) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(lease); err != nil {
		t.Errorf("encode Lease: %v", err)
	}
}

func TestLeaderElectorCreatesMissingLease(t *testing.T) {
	now := time.Date(2026, time.July, 28, 12, 0, 0, 835038584, time.UTC)
	var created kubernetesLease
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/leases/controller":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/leases":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode created Lease: %v", err)
			}
			created.Metadata.ResourceVersion = "1"
			writeLease(t, w, created, http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	elector := testLeaderElector(server, now)
	acquired, err := elector.tryAcquireOrRenew(context.Background())
	if err != nil {
		t.Fatalf("tryAcquireOrRenew() error = %v", err)
	}
	if !acquired {
		t.Fatal("missing Lease was not acquired")
	}
	if created.Spec.HolderIdentity != "controller-0" || created.Metadata.Namespace != "forgejo-runners" {
		t.Fatalf("created Lease = %+v", created)
	}
	if created.Spec.LeaseDurationSeconds != 15 {
		t.Fatalf("leaseDurationSeconds = %d", created.Spec.LeaseDurationSeconds)
	}
	const wantMicroTime = "2026-07-28T12:00:00.835038Z"
	if created.Spec.AcquireTime != wantMicroTime || created.Spec.RenewTime != wantMicroTime {
		t.Fatalf(
			"Lease times = acquire %q, renew %q; want %q",
			created.Spec.AcquireTime,
			created.Spec.RenewTime,
			wantMicroTime,
		)
	}
}

func TestFormatMicroTimeUsesUTCAndSixFractionalDigits(t *testing.T) {
	location := time.FixedZone("test", 9*60*60)
	value := time.Date(2026, time.July, 28, 21, 0, 0, 123456789, location)

	got := formatMicroTime(value)
	const want = "2026-07-28T12:00:00.123456Z"
	if got != want {
		t.Fatalf("formatMicroTime() = %q, want %q", got, want)
	}
	if _, err := time.Parse(microTimeLayout, got); err != nil {
		t.Fatalf("formatMicroTime() returned invalid Kubernetes MicroTime: %v", err)
	}
}

func TestLeaderElectorDoesNotTakeObservedLease(t *testing.T) {
	now := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	lease := kubernetesLease{}
	lease.Metadata.ResourceVersion = "7"
	lease.Spec.HolderIdentity = "controller-1"
	lease.Spec.LeaseDurationSeconds = 15
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		writeLease(t, w, lease, http.StatusOK)
	}))
	defer server.Close()

	elector := testLeaderElector(server, now)
	acquired, err := elector.tryAcquireOrRenew(context.Background())
	if err != nil {
		t.Fatalf("tryAcquireOrRenew() error = %v", err)
	}
	if acquired {
		t.Fatal("unexpired Lease was taken from another controller")
	}
	if !elector.observedTime.Equal(now) {
		t.Fatalf("observedTime = %s, want %s", elector.observedTime, now)
	}
}

func TestLeaderElectorTakesLeaseAfterLocalObservationExpires(t *testing.T) {
	now := time.Date(2026, time.July, 28, 12, 0, 20, 0, time.UTC)
	lease := kubernetesLease{}
	lease.Metadata.ResourceVersion = "7"
	lease.Spec.HolderIdentity = "controller-1"
	lease.Spec.LeaseTransitions = 4
	var updated kubernetesLease
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeLease(t, w, lease, http.StatusOK)
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
				t.Errorf("decode updated Lease: %v", err)
			}
			updated.Metadata.ResourceVersion = "8"
			writeLease(t, w, updated, http.StatusOK)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	elector := testLeaderElector(server, now)
	elector.observedVersion = "7"
	elector.observedTime = now.Add(-defaultLeaseDuration)
	acquired, err := elector.tryAcquireOrRenew(context.Background())
	if err != nil {
		t.Fatalf("tryAcquireOrRenew() error = %v", err)
	}
	if !acquired {
		t.Fatal("expired Lease was not acquired")
	}
	if updated.Spec.HolderIdentity != "controller-0" || updated.Spec.LeaseTransitions != 5 {
		t.Fatalf("updated Lease = %+v", updated)
	}
	if updated.Metadata.ResourceVersion != "8" {
		t.Fatalf("updated resourceVersion = %q", updated.Metadata.ResourceVersion)
	}
}

func TestLeaderElectorDoesNotLeadAfterUpdateConflict(t *testing.T) {
	now := time.Date(2026, time.July, 28, 12, 0, 20, 0, time.UTC)
	lease := kubernetesLease{}
	lease.Metadata.ResourceVersion = "7"
	lease.Spec.HolderIdentity = "controller-1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeLease(t, w, lease, http.StatusOK)
		case http.MethodPut:
			http.Error(w, "conflict", http.StatusConflict)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	elector := testLeaderElector(server, now)
	elector.observedVersion = "7"
	elector.observedTime = now.Add(-defaultLeaseDuration)
	acquired, err := elector.tryAcquireOrRenew(context.Background())
	if err != nil {
		t.Fatalf("tryAcquireOrRenew() error = %v", err)
	}
	if acquired {
		t.Fatal("controller became leader after resourceVersion conflict")
	}
}
