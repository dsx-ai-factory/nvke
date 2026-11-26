// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailedDetachCanBeRetried(t *testing.T) {
	h, id := started(t)
	h.setFile("fail-del", "")
	assert.Equal(t, 1, h.run("stop", "--time", "0", id), "the address was not released")

	meta, err := h.eng.loadContainer(id)
	require.NoError(t, err)
	assert.NotNil(t, meta.CNIResult, "the result stays for the next detach")
	assert.Empty(t, meta.IPv4, "a container that is not attached is not a peer")
	released := len(h.callsMatching("bridge DEL"))

	require.NoError(t, os.Remove(filepath.Join(h.dir, "fake", "fail-del")))
	require.Equal(t, 0, h.run("stop", "--time", "0", id), h.stderr.String())
	assert.Greater(t, len(h.callsMatching("bridge DEL")), released, "the second stop released it again")
	meta, err = h.eng.loadContainer(id)
	require.NoError(t, err)
	assert.Nil(t, meta.CNIResult)
}

func TestFailedAttachIsRolledBack(t *testing.T) {
	h := newHarness(t)
	h.kindNetwork()
	h.setFile("images", "docker.io/library/img:latest application/vnd.oci.image.manifest.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")
	require.Equal(t, 0, h.run("create", "--name", "c", "--network", "kind", "--entrypoint", "/bin/sh", "img", "-c", "sleep"), h.stderr.String())
	id := strings.TrimSpace(h.stdout.String())
	// The hosts file cannot be rewritten, so the attachment cannot be announced to the peers.
	hosts := filepath.Join(h.root, "containers", id, "hosts")
	require.NoError(t, os.Remove(hosts))
	require.NoError(t, os.Mkdir(hosts, 0o755))

	assert.Equal(t, 1, h.run("start", id))
	assert.Contains(t, h.stderr.String(), "hosts")
	assert.Len(t, h.callsMatching("bridge DEL"), 2, "what was left behind, and the attachment that was just made")
	assert.Len(t, h.callsMatching("ip netns del"), 2)
	assert.Empty(t, h.callsMatching("supervisor "))
	meta, err := h.eng.loadContainer(id)
	require.NoError(t, err)
	assert.Nil(t, meta.CNIResult)
	assert.Empty(t, meta.IPv4)
}
