// SPDX-License-Identifier: Apache-2.0

// Package projection converts between resource shapes using matching JSON field names.
// Destination types select the visible fields. Source-only fields are discarded.
// Callers enforce write permissions and preserve field presence when needed for updates.
package projection

import "encoding/json"

// JSON copies fields visible in T and leaves source-only fields behind.
// Callers retaining write intent must preserve original JSON field presence separately.
func JSON[T any](from any) (T, error) {
	var to T
	data, err := json.Marshal(from)
	if err != nil {
		return to, err
	}
	err = json.Unmarshal(data, &to)
	return to, err
}
