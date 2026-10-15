// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package keys_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func TestNewDefaultsAreTheLegacyLiterals(t *testing.T) {
	t.Parallel()

	for _, k := range []keys.Keys{keys.New("", ""), keys.New(keys.LegacyDomain, keys.LegacyManagementNamespace)} {
		assert.Equal(t, "nke.nvidia.com/node-id", k.NodeID.String())
		assert.Equal(t, "nke.nvidia.com/reboot", k.Reboot.String())
		assert.Equal(t, "nke.nvidia.com/reboot", k.MachineRebootAnnotation.String())
		assert.Equal(t, "nke.nvidia.com/maintenance", k.MaintenanceTaint.String())
		assert.Equal(t, "nke.nvidia.com/NeedsRecycle", k.NeedsRecycle.String())
		assert.Equal(t, "nke-system", k.ManagementNamespace)

		// With the default domain there is no second form to read or delete.
		assert.Empty(t, k.Reboot.Legacy())
		assert.Equal(t, []string{"nke.nvidia.com/reboot"}, k.Reboot.All())
	}
}

func TestNewConfiguredDomainWritesNewReadsBoth(t *testing.T) {
	t.Parallel()

	k := keys.New("nvke.example.com", "nvke-system")

	assert.Equal(t, "nvke.example.com/reboot", k.Reboot.String())
	assert.Equal(t, "nke.nvidia.com/reboot", k.Reboot.Legacy())
	assert.Equal(t, []string{"nvke.example.com/reboot", "nke.nvidia.com/reboot"}, k.Reboot.All())
	assert.Equal(t, "nvke-system", k.ManagementNamespace)

	assert.True(t, k.Reboot.Matches("nvke.example.com/reboot"))
	assert.True(t, k.Reboot.Matches("nke.nvidia.com/reboot"))
	assert.False(t, k.Reboot.Matches("other.example.com/reboot"))
}

func TestKeyGet(t *testing.T) {
	t.Parallel()

	k := keys.New("nvke.example.com", "")

	t.Run("reads the legacy form", func(t *testing.T) {
		t.Parallel()
		v, ok := k.Reboot.Get(map[string]string{"nke.nvidia.com/reboot": "legacy"})
		require.True(t, ok)
		assert.Equal(t, "legacy", v)
	})
	t.Run("prefers the configured form", func(t *testing.T) {
		t.Parallel()
		m := map[string]string{"nke.nvidia.com/reboot": "legacy", "nvke.example.com/reboot": "new"}
		assert.Equal(t, "new", k.Reboot.Value(m))
	})
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		v, ok := k.Reboot.Get(map[string]string{"unrelated": "x"})
		assert.False(t, ok)
		assert.Empty(t, v)
		assert.False(t, k.Reboot.Has(nil))
	})
	t.Run("a present empty value counts as present", func(t *testing.T) {
		t.Parallel()
		assert.True(t, k.Reboot.Has(map[string]string{"nke.nvidia.com/reboot": ""}))
	})
	t.Run("default domain does not read an unrelated domain", func(t *testing.T) {
		t.Parallel()
		d := keys.New("", "")
		assert.False(t, d.Reboot.Has(map[string]string{"nvke.example.com/reboot": "x"}))
	})
}

func TestValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, keys.Validate("", ""))
	require.NoError(t, keys.Validate(keys.LegacyDomain, ""))
	require.NoError(t, keys.Validate(keys.NVKEDomain, "nvke-system"))
	require.Error(t, keys.Validate("Not_A_Domain/", ""))
	require.Error(t, keys.Validate("", "Not.A.Namespace"))

	// A syntactically valid domain outside the supported pair is rejected
	// rather than silently splitting the fleet: dual-read only ever looks for
	// LegacyDomain, and CAPNICo can only write these same two, so a third
	// domain is a key nothing else writes and nothing else reads.
	err := keys.Validate("nvke.example.com", "")
	require.ErrorContains(t, err, "unsupported")
	require.ErrorContains(t, err, keys.NVKEDomain)
}

// The keys themselves stay domain-generic: Validate constrains what an operator
// may configure, not what the package can build. Tests elsewhere construct
// keys.New with an arbitrary domain to prove no call site hardcodes NVKEDomain.
func TestNewAcceptsAnyDomainIndependentlyOfValidate(t *testing.T) {
	t.Parallel()

	k := keys.New("nvke.example.com", "")
	require.Equal(t, "nvke.example.com/machine-id", k.MachineID.String())
	require.Equal(t, keys.LegacyDomain+"/machine-id", k.MachineID.Legacy())
	require.Error(t, keys.Validate("nvke.example.com", ""))
}
