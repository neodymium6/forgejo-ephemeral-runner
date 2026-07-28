package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyBlocksCommittedRegistrationUntilReleased(t *testing.T) {
	upstreamCommitted := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/actions/runners") {
			upstreamCommitted <- struct{}{}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(writer, `{"id":42,"uuid":"uuid","token":"token"}`)
			return
		}
		http.NotFound(writer, request)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(newProxyHandler(upstreamURL, log.New(io.Discard, "", 0)))
	defer proxy.Close()

	request(t, http.MethodPost, proxy.URL+controlPathPrefix+"arm", http.StatusNoContent)
	result := make(chan *http.Response, 1)
	requestError := make(chan error, 1)
	go func() {
		response, err := http.Post(proxy.URL+"/api/v1/repos/example/project/actions/runners", "application/json", bytes.NewBufferString(`{"name":"runner"}`))
		if err != nil {
			requestError <- err
			return
		}
		result <- response
	}()

	select {
	case <-upstreamCommitted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream registration was not committed")
	}
	waitForStatus(t, proxy.URL, "blocked")
	select {
	case response := <-result:
		_ = response.Body.Close()
		t.Fatal("proxy returned the registration before release")
	case err := <-requestError:
		t.Fatalf("registration request failed before release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	request(t, http.MethodPost, proxy.URL+controlPathPrefix+"release", http.StatusNoContent)
	select {
	case response := <-result:
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("registration status = %d", response.StatusCode)
		}
	case err := <-requestError:
		t.Fatalf("registration request failed after release: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("registration request remained blocked after release")
	}
	waitForStatus(t, proxy.URL, "idle")
}

func TestProxyRejectsConflictingGateTransitions(t *testing.T) {
	upstreamURL, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(newProxyHandler(upstreamURL, log.New(io.Discard, "", 0)))
	defer proxy.Close()

	request(t, http.MethodPost, proxy.URL+controlPathPrefix+"arm", http.StatusNoContent)
	request(t, http.MethodPost, proxy.URL+controlPathPrefix+"arm", http.StatusConflict)
	request(t, http.MethodPost, proxy.URL+controlPathPrefix+"release", http.StatusConflict)
}

func request(t *testing.T, method, requestURL string, expectedStatus int) {
	t.Helper()
	request, err := http.NewRequest(method, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != expectedStatus {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s returned %d: %s", method, requestURL, response.StatusCode, body)
	}
}

func waitForStatus(t *testing.T, proxyURL, expected string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(proxyURL + controlPathPrefix + "status")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == expected {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("proxy did not reach status %q", expected)
}
