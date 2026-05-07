// Package wmill bridges the untyped Windmill SDK helpers (which return
// interface{}) to typed Go values. Centralizes the JSON-roundtrip pattern
// every Go script uses to deserialize a Windmill resource into a struct.
package wmill

import (
	"encoding/json"
	"fmt"

	sdk "github.com/windmill-labs/windmill-go-client"
)

// GetResource loads the Windmill resource at path and unmarshals it into T
// via JSON roundtrip. The SDK returns interface{} (decoded JSON), so this
// re-marshals it and decodes into the caller's struct — the canonical way
// to consume a typed resource without depending on map[string]any access.
//
// Path errors and JSON errors are wrapped with the resource path for log
// triage.
func GetResource[T any](path string) (T, error) {
	var out T
	raw, err := sdk.GetResource(path)
	if err != nil {
		return out, fmt.Errorf("get resource %s: %w", path, err)
	}
	// Guard against (nil, nil) — the SDK returns this when the path resolves
	// but the value is empty/null. Without this check, json.Marshal(nil)
	// produces "null" and Unmarshal silently zero-values out, hiding the
	// missing-resource case from callers.
	if raw == nil {
		return out, fmt.Errorf("get resource %s: nil value", path)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return out, fmt.Errorf("marshal resource %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("unmarshal resource %s: %w", path, err)
	}
	return out, nil
}
