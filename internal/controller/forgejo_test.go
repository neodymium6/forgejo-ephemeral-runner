package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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
	client := &forgejoClient{httpClient: server.Client(), scopeURL: scopeURL, token: "api-token"}

	runners, err := client.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("ListRunners() error = %v", err)
	}
	if len(runners) != 1 || runners[0].ID != 7 {
		t.Fatalf("ListRunners() = %+v", runners)
	}
	registration, err := client.RegisterRunner(context.Background(), "slot-0", managedDescription)
	if err != nil {
		t.Fatalf("RegisterRunner() error = %v", err)
	}
	wantRegistration := Registration{ID: 42, UUID: "runner-uuid", Token: "one-job-token"}
	if registration != wantRegistration {
		t.Fatalf("RegisterRunner() = %+v, want %+v", registration, wantRegistration)
	}
	if err := client.DeleteRunner(context.Background(), 42); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	if !reflect.DeepEqual(methods, []string{http.MethodGet, http.MethodPost, http.MethodDelete}) {
		t.Fatalf("methods = %v", methods)
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
	client := &forgejoClient{httpClient: server.Client(), scopeURL: scopeURL, token: "token"}
	if err := client.DeleteRunner(context.Background(), 99); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
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
