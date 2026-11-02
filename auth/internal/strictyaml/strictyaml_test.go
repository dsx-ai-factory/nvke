// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package strictyaml_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/internal/strictyaml"
)

type config struct {
	Kind         string   `json:"kind"`
	PathPrefixes []string `json:"pathPrefixes"`
}

func TestDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    config
		wantErr string
	}{
		{name: "one document", raw: "kind: K\npathPrefixes: [/a]\n", want: config{Kind: "K", PathPrefixes: []string{"/a"}}},
		{name: "an empty file", raw: "", wantErr: "test.yaml is empty"},
		{name: "only a comment", raw: "# nothing\n", wantErr: "test.yaml is empty"},
		{name: "two documents", raw: "kind: A\n---\nkind: B\n", wantErr: "test.yaml contains more than one YAML document"},
		{name: "malformed YAML", raw: "kind: [", wantErr: "parse test.yaml: yaml:"},
		{name: "an unknown key", raw: "kind: K\nmode: x\n", wantErr: `parse test.yaml: unknown field "mode"`},
		{
			name:    "a case-mismatched key",
			raw:     "kind: K\npathPrefixes: [/a]\npathprefixes: []\n",
			wantErr: `parse test.yaml: unknown field "pathprefixes"`,
		},
		{
			name:    "a duplicate key",
			raw:     "kind: A\nkind: B\n",
			wantErr: "parse test.yaml: yaml: unmarshal errors:\n" + `  line 2: key "kind" already set in map`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got config
			err := strictyaml.Decode("test.yaml", []byte(tt.raw), &got)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
