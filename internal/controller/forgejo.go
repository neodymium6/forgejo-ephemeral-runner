package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type forgejoClient struct {
	httpClient *http.Client
	scopeURL   *url.URL
	token      string
}

func NewForgejoClient(cfg Config) (Forgejo, error) {
	baseURL, err := url.Parse(cfg.ForgejoURL)
	if err != nil {
		return nil, fmt.Errorf("parse Forgejo URL: %w", err)
	}
	scopePath, err := scopeAPIPath(cfg.ForgejoScope)
	if err != nil {
		return nil, err
	}
	baseURL.Path = path.Join(baseURL.Path, scopePath)
	baseURL.RawQuery = ""
	baseURL.Fragment = ""

	tokenBytes, err := os.ReadFile(cfg.ForgejoAPITokenPath)
	if err != nil {
		return nil, fmt.Errorf("read Forgejo API token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, errors.New("Forgejo API token is empty")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &forgejoClient{
		httpClient: &http.Client{
			Transport:     transport,
			Timeout:       15 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
		scopeURL: baseURL,
		token:    token,
	}, nil
}

func scopeAPIPath(scope string) (string, error) {
	if scope == "user" {
		return "/api/v1/user/actions/runners", nil
	}
	if scope == "instance" {
		return "/api/v1/admin/actions/runners", nil
	}

	kind, value, found := strings.Cut(scope, ":")
	if !found || strings.TrimSpace(value) == "" {
		return "", errors.New("FORGEJO_RUNNER_SCOPE must be user, instance, organization:<name>, or repository:<owner>/<repo>")
	}
	switch kind {
	case "organization":
		if strings.Contains(value, "/") {
			return "", errors.New("organization scope must contain exactly one name")
		}
		return "/api/v1/orgs/" + url.PathEscape(value) + "/actions/runners", nil
	case "repository":
		owner, repository, ok := strings.Cut(value, "/")
		if !ok || owner == "" || repository == "" || strings.Contains(repository, "/") {
			return "", errors.New("repository scope must be repository:<owner>/<repo>")
		}
		return "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository) + "/actions/runners", nil
	default:
		return "", fmt.Errorf("unsupported Forgejo runner scope %q", kind)
	}
}

func (c *forgejoClient) ListJobs(ctx context.Context, labels []string) ([]RemoteJob, error) {
	requestURL := *c.scopeURL
	requestURL.Path = strings.TrimSuffix(requestURL.Path, "/") + "/jobs"
	if len(labels) > 0 {
		query := requestURL.Query()
		query.Set("labels", strings.Join(labels, ","))
		requestURL.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, forgejoResponseError("list jobs", resp)
	}

	var jobs []RemoteJob
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&jobs); err != nil {
		return nil, fmt.Errorf("decode job list: %w", err)
	}
	return jobs, nil
}

func (c *forgejoClient) ListRunners(ctx context.Context) ([]RemoteRunner, error) {
	const pageSize = 50
	var runners []RemoteRunner
	for pageNumber := 1; ; pageNumber++ {
		requestURL := *c.scopeURL
		query := requestURL.Query()
		query.Set("visible", "false")
		query.Set("limit", strconv.Itoa(pageSize))
		query.Set("page", strconv.Itoa(pageNumber))
		requestURL.RawQuery = query.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
		if err != nil {
			return nil, err
		}
		c.authorize(req)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list runners: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			err := forgejoResponseError("list runners", resp)
			return nil, errors.Join(err, resp.Body.Close())
		}

		var page []RemoteRunner
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&page)
		closeErr := resp.Body.Close()
		if err != nil {
			return nil, errors.Join(fmt.Errorf("decode runner list: %w", err), closeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close runner list response: %w", closeErr)
		}
		runners = append(runners, page...)
		if len(page) < pageSize {
			return runners, nil
		}
		if pageNumber >= 100 {
			return nil, errors.New("runner list exceeded 5000 entries")
		}
	}
}

func (c *forgejoClient) RegisterRunner(ctx context.Context, name, description string) (Registration, error) {
	body, err := json.Marshal(struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Ephemeral   bool   `json:"ephemeral"`
	}{Name: name, Description: description, Ephemeral: true})
	if err != nil {
		return Registration{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.scopeURL.String(), bytes.NewReader(body))
	if err != nil {
		return Registration{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Registration{}, fmt.Errorf("register runner: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return Registration{}, forgejoResponseError("register runner", resp)
	}

	var registration Registration
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&registration); err != nil {
		return Registration{}, fmt.Errorf("decode runner registration: %w", err)
	}
	if registration.ID <= 0 || strings.TrimSpace(registration.UUID) == "" || strings.TrimSpace(registration.Token) == "" {
		return Registration{}, errors.New("Forgejo returned an incomplete runner registration")
	}
	return registration, nil
}

func (c *forgejoClient) DeleteRunner(ctx context.Context, id int64) error {
	requestURL := *c.scopeURL
	requestURL.Path = strings.TrimSuffix(requestURL.Path, "/") + "/" + strconv.FormatInt(id, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, requestURL.String(), nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete runner: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return forgejoResponseError("delete runner", resp)
	}
}

func (c *forgejoClient) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultControllerUserAgent)
}

func forgejoResponseError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s returned HTTP %d: %s", operation, resp.StatusCode, strings.TrimSpace(string(body)))
}
