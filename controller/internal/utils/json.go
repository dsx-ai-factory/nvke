// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math/big"
	"slices"
)

// JSONEqual compares JSON content independent of object key order and whitespace.
// Numbers compare by exact value, including equivalent exponent notation.
// Invalid JSON and trailing data are rejected. Nil and empty inputs compare equal.
func JSONEqual(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return len(a) == 0 || json.Valid(a)
	}
	x, errA := decodeJSON(a)
	y, errB := decodeJSON(b)
	return errA == nil && errB == nil && jsonValueEqual(x, y)
}

func decodeJSON(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON value")
	}
	return value, nil
}

func jsonValueEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		r1, ok1 := new(big.Rat).SetString(string(x))
		r2, ok2 := new(big.Rat).SetString(string(y))
		return ok1 && ok2 && r1.Cmp(r2) == 0
	case map[string]any:
		y, ok := b.(map[string]any)
		return ok && maps.EqualFunc(x, y, jsonValueEqual)
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, jsonValueEqual)
	default: // Only strings, booleans and nil remain after decoding.
		return a == b
	}
}
