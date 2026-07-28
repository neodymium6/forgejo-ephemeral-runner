package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const controlPathPrefix = "/__e2e/"

type registrationGate struct {
	mu      sync.Mutex
	state   string
	release chan struct{}
}

func newRegistrationGate() *registrationGate {
	return &registrationGate{state: "idle"}
}

func (g *registrationGate) arm() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != "idle" {
		return fmt.Errorf("registration gate is %s", g.state)
	}
	g.state = "armed"
	g.release = make(chan struct{})
	return nil
}

func (g *registrationGate) blockIfArmed(ctx context.Context) error {
	g.mu.Lock()
	if g.state != "armed" {
		g.mu.Unlock()
		return nil
	}
	g.state = "blocked"
	release := g.release
	g.mu.Unlock()

	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *registrationGate) releaseBlocked() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != "blocked" {
		return fmt.Errorf("registration gate is %s", g.state)
	}
	close(g.release)
	g.release = nil
	g.state = "idle"
	return nil
}

func (g *registrationGate) status() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

func newProxyHandler(upstream *url.URL, logger *log.Logger) http.Handler {
	gate := newRegistrationGate()
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorLog = logger
	proxy.ModifyResponse = func(response *http.Response) error {
		request := response.Request
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/actions/runners") {
			return gate.blockIfArmed(request.Context())
		}
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /__e2e/health", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /__e2e/status", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(writer, gate.status())
	})
	mux.HandleFunc("POST /__e2e/arm", func(writer http.ResponseWriter, _ *http.Request) {
		if err := gate.arm(); err != nil {
			http.Error(writer, err.Error(), http.StatusConflict)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__e2e/release", func(writer http.ResponseWriter, _ *http.Request) {
		if err := gate.releaseBlocked(); err != nil {
			http.Error(writer, err.Error(), http.StatusConflict)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", proxy)
	return mux
}

func main() {
	logger := log.New(os.Stdout, "e2e-proxy: ", log.LstdFlags|log.LUTC)
	upstreamRaw := strings.TrimSpace(os.Getenv("UPSTREAM_URL"))
	if upstreamRaw == "" {
		logger.Fatal("UPSTREAM_URL is required")
	}
	upstream, err := url.Parse(upstreamRaw)
	if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") {
		logger.Fatal("UPSTREAM_URL must be an absolute HTTP(S) URL")
	}
	listenAddress := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddress == "" {
		listenAddress = ":3001"
	}
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           newProxyHandler(upstream, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger.Printf("listening on %s", listenAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
}
