// Package socialchecker is a thin client for the internal social-checker
// service, which verifies whether a list of social handles are banned on
// their respective platforms. The default URL targets the in-cluster
// Docker DNS name; override Client.URL when calling from outside the
// shared-net network or for tests.
package socialchecker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DefaultURL is the in-cluster endpoint the Windmill workers reach via the
// shared-net Docker network.
const DefaultURL = "http://social-checker:8080/v1/check"

// errBodyMaxBytes caps how much of an error response body is included in the
// error string, to keep job logs readable when upstream returns a giant
// HTML/JSON error page.
const errBodyMaxBytes = 512

// CheckRequest is one (platform, username) pair to verify.
type CheckRequest struct {
	Platform string `json:"platform"`
	Username string `json:"username"`
}

// CheckResult is the verdict for one CheckRequest. IsBanned == true means
// the upstream platform reports the account as banned/suspended/removed.
type CheckResult struct {
	Platform string `json:"platform"`
	Username string `json:"username"`
	IsBanned bool   `json:"is_banned"`
}

// Client is a social-checker HTTP client. The zero value is usable and
// targets DefaultURL via http.DefaultClient. Set URL/HTTP to override.
type Client struct {
	URL  string
	HTTP *http.Client
}

// Check posts the given checks and returns the per-check results. The caller
// is expected to set a deadline on ctx if the upstream may stall — the client
// itself imposes none. Returns an error if the response is missing the
// "results" field while checks were submitted (silent-failure guard for
// upstreams that 200 with an unexpected payload shape).
func (c *Client) Check(ctx context.Context, checks []CheckRequest) ([]CheckResult, error) {
	body, err := json.Marshal(map[string]any{"checks": checks})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.urlOrDefault(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpOrDefault().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncateBody(respBody))
	}

	// Use a pointer so we can distinguish "results: []" (empty array, valid)
	// from "results omitted" (null/missing, treat as upstream contract break).
	var parsed struct {
		Results *[]CheckResult `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode: %w (body=%s)", err, truncateBody(respBody))
	}
	if parsed.Results == nil {
		if len(checks) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("decode: missing 'results' field (body=%s)", truncateBody(respBody))
	}
	return *parsed.Results, nil
}

// Check is a convenience wrapper for callers that don't need to override
// URL or HTTP client — equivalent to (&Client{}).Check(ctx, checks).
func Check(ctx context.Context, checks []CheckRequest) ([]CheckResult, error) {
	return (&Client{}).Check(ctx, checks)
}

func (c *Client) urlOrDefault() string {
	if c.URL == "" {
		return DefaultURL
	}
	return c.URL
}

func (c *Client) httpOrDefault() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}

func truncateBody(b []byte) []byte {
	if len(b) <= errBodyMaxBytes {
		return b
	}
	out := make([]byte, 0, errBodyMaxBytes+len("...(truncated)"))
	out = append(out, b[:errBodyMaxBytes]...)
	out = append(out, "...(truncated)"...)
	return out
}
