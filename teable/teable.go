// Package teable is a slim, dependency-free client for the Teable native REST
// API (https://teable.fanworkz.com/docs). Mirrors the shape of helpers/airtable
// so callers can swap one for the other with a mechanical import + type swap.
//
// Two intentional differences from the in-repo reddit-client teablex:
//   - Base URL is passed to the constructor rather than read from env, so the
//     helper composes cleanly with Windmill's resource model.
//   - There is no ListFieldNames call; Teable PATs scoped to record read/write
//     typically lack field-read permission. Callers always pass an explicit
//     projection to FetchRecordsFromView.
package teable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FanWorkz/windmill-helpers/wmill"
)

const (
	// maxTake is Teable's documented per-request cap on GET /record (`take`).
	maxTake = 1000

	// batchUpdateLimit caps records per PATCH /record. Teable doesn't publish a
	// hard cap, but a low number keeps body size bounded and limits the blast
	// radius of a partial server error.
	batchUpdateLimit = 100

	// maxSearchPages is a safety bound. A single Reddit base shouldn't approach
	// this — bump if a real table outgrows it.
	maxSearchPages = 1000

	// fieldKeyName instructs Teable to key record.fields by field NAME rather
	// than field id, so callers can use the human-readable column names that
	// match the Airtable migration.
	fieldKeyName = "name"

	// userAgent is sent on every request. Teable's public edge sits behind
	// Cloudflare bot protection that rejects Go's default UA with HTTP 403
	// (CF error 1010); a browser UA passes. Sending it unconditionally also
	// works for internal/Docker callers that skip the edge.
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

	defaultBaseURL    = "https://teable.fanworkz.com"
	defaultHTTPClient = 60 * time.Second
)

// Record is a Teable record returned by GET /record and accepted by PATCH /record.
// Fields is the untyped cell-value map; cell parsing is the caller's job.
type Record struct {
	ID     string         `json:"id,omitempty"`
	Name   string         `json:"name,omitempty"`
	Fields map[string]any `json:"fields"`
}

// StringField returns the named field as a string, or "" if missing or
// non-string. Convenience for the untyped Fields map — single-line cells are
// typically absent (not "") when blank, and non-string cell types are rejected
// silently rather than coerced.
func StringField(r *Record, name string) string {
	v, ok := r.Fields[name]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// Client talks to a single Teable instance.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithHTTPClient routes Teable requests through the supplied http.Client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) { c.http = httpClient }
}

// New builds a Teable client. baseURL is the instance root (e.g.
// https://teable.fanworkz.com); a trailing /api or / is trimmed automatically.
// Empty baseURL falls back to the public host.
func New(token, baseURL string, opts ...Option) *Client {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = defaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/api")
	c := &Client{
		baseURL: base,
		token:   token,
		http:    &http.Client{Timeout: defaultHTTPClient},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// resourceShape mirrors the Windmill Teable resource: a bearer token plus the
// instance base URL. baseUrl is optional; empty value falls back to the public
// host (see New).
type resourceShape struct {
	Token   string `json:"token"`
	BaseURL string `json:"baseUrl"`
}

// NewFromResource loads the Windmill Teable resource at path and constructs a
// Client from its token + baseUrl fields. Returns an error if the resource is
// missing, can't be deserialized, or has an empty token.
func NewFromResource(path string, opts ...Option) (*Client, error) {
	res, err := wmill.GetResource[resourceShape](path)
	if err != nil {
		return nil, err
	}
	if res.Token == "" {
		return nil, fmt.Errorf("teable resource %s: empty token", path)
	}
	return New(res.Token, res.BaseURL, opts...), nil
}

// do executes an authenticated JSON request against /api{path}. body may be nil.
// When discardBody is true, the response body is read and dropped — used for
// PATCH /record, whose response echoes every field on the record (including
// secrets like Session Cookie) and shouldn't enter the caller's logs.
func (c *Client) do(
	ctx context.Context,
	method, path string,
	query url.Values,
	body any,
	out any,
) error {
	u := c.baseURL + "/api" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// FetchRecordsFromView pages through every record in viewID, projecting to the
// given fields. fields is required: Teable PATs scoped to record-only access
// can't list field names server-side, so the caller must enumerate columns.
// Returns records in view order.
func (c *Client) FetchRecordsFromView(
	ctx context.Context,
	tableID, viewID string,
	fields ...string,
) ([]*Record, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("teable fetch (table=%s view=%s): fields list required", tableID, viewID)
	}
	var (
		all  []*Record
		skip int
	)
	for range maxSearchPages {
		query := url.Values{}
		query.Set("fieldKeyType", fieldKeyName)
		query.Set("take", strconv.Itoa(maxTake))
		query.Set("skip", strconv.Itoa(skip))
		if viewID != "" {
			query.Set("viewId", viewID)
		}
		for _, f := range fields {
			query.Add("projection[]", f)
		}

		var page struct {
			Records []*Record `json:"records"`
		}
		if err := c.do(ctx, http.MethodGet,
			"/table/"+tableID+"/record", query, nil, &page); err != nil {
			return nil, fmt.Errorf("teable fetch (table=%s view=%s skip=%d): %w",
				tableID, viewID, skip, err)
		}
		all = append(all, page.Records...)
		if len(page.Records) < maxTake {
			return all, nil
		}
		skip += maxTake
	}
	return nil, fmt.Errorf("teable pagination exceeded %d pages (table=%s view=%s)",
		maxSearchPages, tableID, viewID)
}

// updatePayload is the batch PATCH body for /table/{id}/record.
type updatePayload struct {
	FieldKeyType string    `json:"fieldKeyType"`
	Typecast     bool      `json:"typecast"`
	Records      []*Record `json:"records"`
}

// BatchUpdate applies a partial PATCH to every record in records (Teable
// PATCH is inherently partial — unspecified fields are left untouched).
// Each record must have ID and Fields set. Records are sent in chunks of
// batchUpdateLimit; the call returns on the first error.
//
// Typecast=true is passed so the server coerces ISO8601 strings into the
// receiving column's native datetime type — caller-side formatting only
// needs to be valid ISO8601 (e.g. time.RFC3339).
//
// The response body is intentionally discarded: Teable echoes the entire
// updated record (including every other field on it, e.g. auth tokens and
// session cookies) and that data has no business reaching the caller's logs.
func (c *Client) BatchUpdate(
	ctx context.Context,
	tableID string,
	records []*Record,
) error {
	for i := 0; i < len(records); i += batchUpdateLimit {
		end := min(i+batchUpdateLimit, len(records))
		body := updatePayload{
			FieldKeyType: fieldKeyName,
			Typecast:     true,
			Records:      records[i:end],
		}
		if err := c.do(ctx, http.MethodPatch,
			"/table/"+tableID+"/record", nil, body, nil); err != nil {
			return fmt.Errorf("teable batch update (table=%s offset=%d): %w",
				tableID, i, err)
		}
	}
	return nil
}
