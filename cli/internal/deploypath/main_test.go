// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"os"
	"testing"
)

// TestMain clears the SPIRE CA variables an operator's shell may export, so
// specs without spire.upstreamCA load as the tests expect; the infra provider,
// SPIRE address and node list overrides, which turn every spec into a nico one
// or reject it; the controller port, which the tests expect at its default; and
// the credentials' NKECTL_ aliases, which a test blanking an NVKECTL_ name would
// fall back to.
func TestMain(m *testing.M) {
	for _, name := range []string{
		"NVKECTL_SPIRE_CA_CERT_FILE", "NVKECTL_SPIRE_CA_KEY_FILE",
		"NKECTL_SPIRE_CA_CERT_FILE", "NKECTL_SPIRE_CA_KEY_FILE",
		"SPIRE_CA_CERT_FILE", "SPIRE_CA_KEY_FILE",
		"NKECTL_NICO_TOKEN", "NKECTL_NODE_USER", "NKECTL_NODE_PASSWORD", "NKECTL_DATABASE_PASSWORD",
		"NVKECTL_INFRA_PROVIDER", "NKECTL_INFRA_PROVIDER",
		"NVKECTL_SPIRE_EXTERNAL_ADDRESS", "NKECTL_SPIRE_EXTERNAL_ADDRESS",
		"NKECTL_NICO_CREDENTIALS",
		"NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT", "NKECTL_NICO_ALLOW_INSECURE_ENDPOINT",
		"NVKECTL_CLUSTER_NODES", "NKECTL_CLUSTER_NODES",
		"NVKECTL_NVKE_CONTROLLER_PORT", "NVKECTL_CONTROLLER_PORT", "NKECTL_CONTROLLER_PORT",
	} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
