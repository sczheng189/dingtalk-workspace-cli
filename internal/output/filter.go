// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	apperrors "github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/errors"
	"github.com/itchyny/gojq"
)

// SelectFields filters a JSON-serialisable payload to include only
// the specified field names. It works on both objects and arrays:
//   - Object: returns a new object with only the matching keys.
//   - Array of objects: returns a new array where each element
//     contains only the matching keys.
//   - Other types: returned unchanged.
//
// Field names are matched case-insensitively against top-level keys.
// Nested field selection (e.g. "response.data") is not supported;
// use --jq for complex queries.
func SelectFields(payload any, fields []string) any {
	if len(fields) == 0 {
		return payload
	}

	// Normalise to a generic JSON structure.
	normalised := toGeneric(payload)

	wanted := make(map[string]bool, len(fields))
	for _, f := range fields {
		wanted[strings.TrimSpace(strings.ToLower(f))] = true
	}

	switch typed := normalised.(type) {
	case map[string]any:
		return filterMap(typed, wanted)
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]any); ok {
				result = append(result, filterMap(m, wanted))
			} else {
				result = append(result, item)
			}
		}
		return result
	default:
		return normalised
	}
}

// ApplyJQ applies a jq expression to a JSON-serialisable payload and
// writes the results to w. Each result value is written as a separate
// line of JSON.
//
// The expression is compiled once and evaluated against the
// normalised payload. Multiple result values (e.g. from `.[]`) are
// each written as indented JSON followed by a newline.
func ApplyJQ(w io.Writer, payload any, expr string) error {
	query, err := gojq.Parse(expr)
	if err != nil {
		return apperrors.NewValidation(fmt.Sprintf("invalid --jq expression: %v", err))
	}

	normalised := toGeneric(payload)
	iter := query.Run(normalised)

	first := true
	for {
		value, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := value.(error); isErr {
			return apperrors.NewValidation(fmt.Sprintf("--jq evaluation error: %v", err))
		}
		if !first {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		first = false

		data, marshalErr := json.MarshalIndent(value, "", "  ")
		if marshalErr != nil {
			return apperrors.NewInternal("failed to encode --jq result")
		}
		if _, err := fmt.Fprint(w, string(data)); err != nil {
			return err
		}
	}
	if !first {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}

// filterMap returns a new map containing only keys that match the
// wanted set (case-insensitive).
func filterMap(m map[string]any, wanted map[string]bool) map[string]any {
	result := make(map[string]any, len(wanted))
	for key, value := range m {
		if wanted[strings.ToLower(key)] {
			result[key] = value
		}
	}
	return result
}

// toGeneric converts an arbitrary Go value into a generic JSON
// structure (map[string]any / []any / primitives) by round-tripping
// through JSON marshal/unmarshal. This ensures consistent types
// regardless of the original Go struct. The round-trip is always
// performed because even a map[string]any may contain typed values
// (e.g. []ir.CanonicalProduct) that gojq cannot handle.
func toGeneric(payload any) any {
	if payload == nil {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return payload
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return payload
	}
	return generic
}
