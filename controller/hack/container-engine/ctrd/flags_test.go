// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFlags(t *testing.T) {
	specs := flagSet{
		"name":    {Kind: stringFlag},
		"label":   {Kind: listFlag, Short: "l"},
		"all":     {Kind: boolFlag, Short: "a"},
		"quiet":   {Kind: boolFlag, Short: "q"},
		"init":    {Kind: boolFlag},
		"format":  {Kind: stringFlag, Short: "f"},
		"restart": {Kind: stringFlag},
	}
	tests := []struct {
		name       string
		args       []string
		stopAtArg  bool
		wantBools  map[string]bool
		wantStrs   map[string]string
		wantLists  map[string][]string
		wantArgs   []string
		wantErrMsg string
	}{
		{
			name: "long flags with a value, with and without equals", args: []string{"--name", "a", "--restart=on-failure:1", "x"},
			wantStrs: map[string]string{"name": "a", "restart": "on-failure:1"}, wantArgs: []string{"x"},
		},
		{
			name: "short flags and a run of boolean ones", args: []string{"-aq", "-f", "{{.Names}}"},
			wantBools: map[string]bool{"all": true, "quiet": true}, wantStrs: map[string]string{"format": "{{.Names}}"},
		},
		{
			name: "a short flag with an equals sign", args: []string{"-f={{.ID}}"}, wantStrs: map[string]string{"format": "{{.ID}}"},
		},
		{
			name: "list flags keep their order", args: []string{"--label", "b=2", "-l", "a=1"},
			wantLists: map[string][]string{"label": {"b=2", "a=1"}},
		},
		{
			name: "a boolean flag can be turned off", args: []string{"--init=false"}, wantBools: map[string]bool{"init": false},
		},
		{
			name: "flags may follow arguments", args: []string{"net", "--format={{.Name}}"},
			wantStrs: map[string]string{"format": "{{.Name}}"}, wantArgs: []string{"net"},
		},
		{
			name: "the first argument ends the flags when asked", args: []string{"--name", "n", "image", "-c", "--name", "script"}, stopAtArg: true,
			wantStrs: map[string]string{"name": "n"}, wantArgs: []string{"image", "-c", "--name", "script"},
		},
		{
			name: "a double dash ends the flags", args: []string{"--", "--name"}, wantArgs: []string{"--name"},
		},
		{name: "an unknown long flag is not supported", args: []string{"--volumes-from", "x"}, wantErrMsg: "t --volumes-from: not supported by this engine"},
		{name: "an unknown short flag is not supported", args: []string{"-z"}, wantErrMsg: "t -z: not supported by this engine"},
		{name: "a value flag in a run of short flags is not supported", args: []string{"-af"}, wantErrMsg: "not supported by this engine"},
		{name: "a missing value is an error", args: []string{"--name"}, wantErrMsg: "needs an argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFlags("t", specs, tt.args, tt.stopAtArg)
			if tt.wantErrMsg != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrMsg)
				return
			}
			require.NoError(t, err)
			for name, want := range tt.wantBools {
				assert.Equal(t, want, got.has(name), name)
			}
			for name, want := range tt.wantStrs {
				assert.Equal(t, want, got.str(name), name)
			}
			for name, want := range tt.wantLists {
				assert.Equal(t, want, got.list(name), name)
			}
			assert.Equal(t, tt.wantArgs, got.args)
		})
	}
}
