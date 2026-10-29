// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
)

func manualArgs() []string {
	return []string{
		"--action", "renew", "--cluster", "example-cluster", "--node", "example-node",
		"--operation", "example-run", "--actor", "example-operator", "--outcome", "success", "--reason", "completed",
	}
}

func TestManualEvidenceUsesSharedSchemaAndHasStableID(t *testing.T) {
	var first, second bytes.Buffer
	require.NoError(t, run(manualArgs(), &first))
	require.NoError(t, run(manualArgs(), &second))
	var event, repeat credentialaudit.Event
	require.NoError(t, json.Unmarshal(first.Bytes(), &event))
	require.NoError(t, json.Unmarshal(second.Bytes(), &repeat))
	require.NoError(t, event.Validate())
	require.Equal(t, event.ID, repeat.ID)
	require.Equal(t, credentialaudit.PlaneT1, event.Plane)
	require.Equal(t, credentialaudit.ActorOperatorReported, event.Actor.Kind)
	require.Equal(t, credentialaudit.Hash("actor", "example-operator"), event.Actor.SubjectHash)
	require.Equal(t, credentialaudit.Hash("cluster", "example-cluster"), event.Target.ClusterHash)
	require.NotContains(t, first.String(), "example-operator")
	require.NotContains(t, first.String(), "example-cluster")
	require.NotContains(t, first.String(), "example-node")
	require.NotContains(t, first.String(), "example-run")
}

func TestManualEvidenceRejectsUnboundedInputsWithoutEcho(t *testing.T) {
	for _, args := range [][]string{
		append(manualArgs(), "--reason", "example-token"),
		append(manualArgs(), "--timestamp", "example-token"),
		append(manualArgs(), "--unknown-example-token"),
		append(manualArgs(), "--cluster-id", "example-token"),
		append(manualArgs(), "--certificate", "/missing/example-token"),
	} {
		var output bytes.Buffer
		err := run(args, &output)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "example-token")
		require.Empty(t, output.String())
	}
}

func TestManualEvidenceRejectsKeyInputAndOutputFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public.pem")
	require.NoError(t, os.WriteFile(path, []byte("-----BEGIN PRIVATE KEY-----\nREDACTED\n-----END PRIVATE KEY-----\n"), 0o600))
	var output bytes.Buffer
	err := run(append(manualArgs(), "--certificate", path), &output)
	require.Error(t, err)
	require.Empty(t, output.String())
	require.NotContains(t, err.Error(), "REDACTED")
	err = run(manualArgs(), failingOutput{})
	require.EqualError(t, err, "could not write audit evidence")
}

type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) {
	return 0, errors.New("example-token")
}
