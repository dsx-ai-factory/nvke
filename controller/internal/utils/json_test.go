// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONEqual(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, desired, current string
		want                   bool
	}{
		{"exponent", `{"spec":{"value":1e-7}}`, `{"spec":{"value":0.0000001}}`, true},
		{"nested numbers", `{"values":[1,1e3,-0,true,null,"x"]}`, `{"values":[1.0,1000,0,true,null,"x"]}`, true},
		{"large equivalent integers", `{"value":9007199254740993}`, `{"value":9007199254740993.0}`, true},
		{"distinct large integers", `{"value":9007199254740992}`, `{"value":9007199254740993}`, false},
		{"number and string", `{"value":1}`, `{"value":"1"}`, false},
		{"array order", `[1,2]`, `[2,1]`, false},
		{"object keys", `{"a":null}`, `{"b":null}`, false},
		{"invalid JSON", `{"value":`, `{"value":1}`, false},
		{"identical bytes", `{"a":1}`, `{"a":1}`, true},
		{"key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"nested key order", `{"a":{"b":1,"c":2}}`, `{"a":{"c":2,"b":1}}`, true},
		{"whitespace", ` { "a" : [1, 2] } `, `{"a":[1,2]}`, true},
		{"object and array", `{}`, `[]`, false},
		{"true and false", `true`, `false`, false},
		{"null and string", `null`, `"null"`, false},
		{"identical invalid JSON", `{"a":`, `{"a":`, false},
		{"trailing garbage", `{"a":1} garbage`, `{"a":1}`, false},
		{"trailing JSON value", `{"a":1} {}`, `{"a":1}`, false},
		{"trailing delimiter", `{"a":1}]`, `{"a":1}`, false},
		{"identical trailing data", `{"a":1} garbage`, `{"a":1} garbage`, false},
		{"trailing whitespace", "{\"a\":1} \n\t", `{"a":1}`, true},
		{"empty inputs", ``, ``, true},
		{"empty and null", ``, `null`, false},
		{"whitespace only", ` `, ` `, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, JSONEqual(json.RawMessage(tt.desired), json.RawMessage(tt.current)))
			require.Equal(t, tt.want, JSONEqual(json.RawMessage(tt.current), json.RawMessage(tt.desired)))
		})
	}
	t.Run("nil and empty", func(t *testing.T) {
		t.Parallel()
		require.True(t, JSONEqual(nil, json.RawMessage{}))
		require.True(t, JSONEqual(json.RawMessage{}, nil))
		require.True(t, JSONEqual(nil, nil))
	})
}
