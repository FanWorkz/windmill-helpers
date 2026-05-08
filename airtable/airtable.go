// Package airtable wraps mehanizm/airtable with helpers for paginated search
// and chunked batch update (Airtable caps writes at 10 records per request).
package airtable

import (
	"context"
	"fmt"
	"net/http"

	"github.com/FanWorkz/windmill-helpers/wmill"
	upstream "github.com/mehanizm/airtable"
)

const (
	batchUpdateLimit = 10
	maxSearchPages   = 1000 // safety bound; tune up if any base actually exceeds this
)

// Record is a re-export of the upstream Record type so callers don't need to
// import the upstream package directly.
type Record = upstream.Record

// StringField returns the named field as a string, or "" if missing or
// non-string. Convenience for Airtable's interface{}-typed Fields map —
// blank single-line-text cells are typically absent rather than "", and
// non-string cell types (numbers, attachments) are rejected silently.
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

// Client wraps an Airtable API client.
type Client struct {
	api *upstream.Client
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithHTTPClient routes Airtable requests through the supplied http.Client.
// Useful for injecting custom timeouts, transports, or test stubs.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		c.api.SetCustomClient(httpClient)
	}
}

// New constructs a Client using the given Airtable Personal Access Token.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{api: upstream.NewClient(apiKey)}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// resourceShape mirrors the Windmill Airtable resource type, which stores
// the PAT under "apiKey".
type resourceShape struct {
	APIKey string `json:"apiKey"`
}

// NewFromResource loads the Windmill Airtable resource at path and constructs
// a Client from its apiKey field. Returns an error if the resource is missing,
// can't be deserialized, or has an empty apiKey.
func NewFromResource(path string, opts ...Option) (*Client, error) {
	res, err := wmill.GetResource[resourceShape](path)
	if err != nil {
		return nil, err
	}
	if res.APIKey == "" {
		return nil, fmt.Errorf("airtable resource %s: empty apiKey", path)
	}
	return New(res.APIKey, opts...), nil
}

// SearchAll returns every record matching filterFormula across all pages.
// fields is the list of column names to return (empty = all default fields).
// Returns an error if pagination exceeds maxSearchPages or if the upstream
// returns the same offset twice in a row (indicating a buggy server response).
func (c *Client) SearchAll(
	ctx context.Context,
	baseID, tableName, filterFormula string,
	fields ...string,
) ([]*Record, error) {
	table := c.api.GetTable(baseID, tableName)
	var (
		all        []*Record
		offset     string
		prevOffset string
	)
	for range maxSearchPages {
		cfg := table.GetRecords().WithFilterFormula(filterFormula)
		if len(fields) > 0 {
			cfg = cfg.ReturnFields(fields...)
		}
		if offset != "" {
			cfg = cfg.WithOffset(offset)
		}
		result, err := cfg.DoContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("airtable get records (base=%s table=%s): %w", baseID, tableName, err)
		}
		all = append(all, result.Records...)
		if result.Offset == "" {
			return all, nil
		}
		if result.Offset == prevOffset {
			return nil, fmt.Errorf("airtable pagination stuck: offset %q repeated (base=%s table=%s)", result.Offset, baseID, tableName)
		}
		prevOffset = offset
		offset = result.Offset
	}
	return nil, fmt.Errorf("airtable pagination exceeded %d pages (base=%s table=%s)", maxSearchPages, baseID, tableName)
}

// BatchUpdate applies UpdateRecordsPartial to all records, automatically
// chunking into requests of <= 10 records (Airtable per-request cap). Each
// record must have ID and Fields set. Returns the post-update records as
// reported by Airtable, in the same order as the input.
func (c *Client) BatchUpdate(
	ctx context.Context,
	baseID, tableName string,
	records []*Record,
) ([]*Record, error) {
	if len(records) == 0 {
		return nil, nil
	}
	table := c.api.GetTable(baseID, tableName)
	updated := make([]*Record, 0, len(records))
	for i := 0; i < len(records); i += batchUpdateLimit {
		end := min(i+batchUpdateLimit, len(records))
		chunk := &upstream.Records{Records: records[i:end]}
		resp, err := table.UpdateRecordsPartialContext(ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("airtable batch update (base=%s offset=%d): %w", baseID, i, err)
		}
		updated = append(updated, resp.Records...)
	}
	return updated, nil
}
