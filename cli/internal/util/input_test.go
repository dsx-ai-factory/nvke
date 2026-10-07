// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadFileOrStdin(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "doc.yaml")
	require.NoError(t, os.WriteFile(path, []byte("from file"), 0o600))

	tests := []struct {
		name    string
		stdin   string
		path    string
		want    string
		wantErr string
	}{
		{"a file", "unused", path, "from file", ""},
		{"stdin", "from stdin", "-", "from stdin", ""},
		{"a missing file", "", filepath.Join(t.TempDir(), "nope.yaml"), "", "opening"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ReadFileOrStdin(strings.NewReader(tt.stdin), tt.path, "spec")

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestReadFileOrStdinNamesTheInputWhenReadingFails(t *testing.T) {
	t.Parallel()

	_, err := ReadFileOrStdin(failingReader{}, "-", "patch")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading patch: boom")
}
