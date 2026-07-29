package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestForgejoClientLifecycle(t *testing.T) {
	const apiPath = "/api/v1/repos/example/project/actions/runners"
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer api-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		methods = append(methods, r.Method)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == apiPath+"/jobs":
			if got := r.URL.Query().Get("labels"); got != "linux-amd64:host,nix" {
				t.Errorf("labels query = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":11,"attempt":1,"handle":"waiting-handle","runs_on":["linux-amd64:host"],"status":"waiting"},
				{"id":12,"attempt":2,"handle":"running-handle","runs_on":["nix"],"status":"running"}
			]`))
		case r.Method == http.MethodGet && r.URL.Path == apiPath:
			if r.URL.Query().Get("visible") != "false" {
				t.Errorf("visible query = %q, want false", r.URL.Query().Get("visible"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":7,"name":"other","description":"other","ephemeral":true}]`))
		case r.Method == http.MethodPost && r.URL.Path == apiPath:
			var body struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Ephemeral   bool   `json:"ephemeral"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if body.Name != "slot-0" || body.Description != managedDescription || !body.Ephemeral {
				t.Errorf("unexpected registration body: %+v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":42,"uuid":"runner-uuid","token":"one-job-token"}`))
		case r.Method == http.MethodDelete && r.URL.Path == apiPath+"/42":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	scopeURL, err := url.Parse(server.URL + apiPath)
	if err != nil {
		t.Fatal(err)
	}
	client := &forgejoClient{
		httpClient: server.Client(),
		scopeURLs:  map[string]*url.URL{testScope: scopeURL},
		scopes:     []string{testScope},
		token:      "api-token",
	}

	jobs, err := client.ListJobs(context.Background(), []string{"linux-amd64:host", "nix"})
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	wantJobs := []RemoteJob{
		{Scope: testScope, ID: 11, Attempt: 1, Handle: "waiting-handle", RunsOn: []string{"linux-amd64:host"}, Status: "waiting"},
		{Scope: testScope, ID: 12, Attempt: 2, Handle: "running-handle", RunsOn: []string{"nix"}, Status: "running"},
	}
	if !reflect.DeepEqual(jobs, wantJobs) {
		t.Fatalf("ListJobs() = %+v, want %+v", jobs, wantJobs)
	}

	runners, err := client.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("ListRunners() error = %v", err)
	}
	if len(runners) != 1 || runners[0].ID != 7 {
		t.Fatalf("ListRunners() = %+v", runners)
	}
	registration, err := client.RegisterRunner(context.Background(), testScope, "slot-0", managedDescription)
	if err != nil {
		t.Fatalf("RegisterRunner() error = %v", err)
	}
	wantRegistration := Registration{ID: 42, UUID: "runner-uuid", Token: "one-job-token"}
	if registration != wantRegistration {
		t.Fatalf("RegisterRunner() = %+v, want %+v", registration, wantRegistration)
	}
	if err := client.DeleteRunner(context.Background(), testScope, 42); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	if !reflect.DeepEqual(methods, []string{http.MethodGet, http.MethodGet, http.MethodPost, http.MethodDelete}) {
		t.Fatalf("methods = %v", methods)
	}
}

func TestNewForgejoClientBuildsAllowlistedScopes(t *testing.T) {
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("one-token"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	tests := []struct {
		name      string
		allowlist []string
		wantPaths map[string]string
	}{
		{name: "repositories", allowlist: []string{"example/one", "example/two"}, wantPaths: map[string]string{"example/one": "/api/v1/repos/example/one/actions/runners", "example/two": "/api/v1/repos/example/two/actions/runners"}},
		{name: "wildcard", allowlist: []string{"*"}, wantPaths: map[string]string{"*": "/api/v1/user/actions/runners"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forgejo, err := NewForgejoClient(Config{
				ForgejoURL: "https://forgejo.example.com", ForgejoAPITokenPath: tokenPath,
				ForgejoRepositoryAllowlist: test.allowlist,
			})
			if err != nil {
				t.Fatalf("NewForgejoClient() error = %v", err)
			}
			client, ok := forgejo.(*forgejoClient)
			if !ok {
				t.Fatalf("NewForgejoClient() type = %T", forgejo)
			}
			for scope, wantPath := range test.wantPaths {
				if got := client.scopeURLs[scope].Path; got != wantPath {
					t.Fatalf("scope %q path = %q, want %q", scope, got, wantPath)
				}
			}
		})
	}
}

func TestDeleteRunnerAcceptsNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	scopeURL, err := url.Parse(server.URL + "/api/v1/user/actions/runners")
	if err != nil {
		t.Fatal(err)
	}
	client := &forgejoClient{
		httpClient: server.Client(),
		scopeURLs:  map[string]*url.URL{"*": scopeURL},
		scopes:     []string{"*"},
		token:      "token",
	}
	if err := client.DeleteRunner(context.Background(), "*", 99); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
}

func TestForgejoResponseErrorOmitsResponseBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(strings.NewReader("sensitive upstream response")),
	}
	err := forgejoResponseError("list jobs", resp)
	if got, want := err.Error(), "list jobs returned HTTP 502"; got != want {
		t.Fatalf("forgejoResponseError() = %q, want %q", got, want)
	}
}

func TestParseRunnerID(t *testing.T) {
	if id, err := parseRunnerID("42"); err != nil || id != 42 {
		t.Fatalf("parseRunnerID() = %d, %v", id, err)
	}
	for _, value := range []string{"", "0", "-1", "not-a-number", "42 trailing"} {
		if _, err := parseRunnerID(value); err == nil {
			t.Fatalf("parseRunnerID(%q) succeeded", value)
		}
	}
}
