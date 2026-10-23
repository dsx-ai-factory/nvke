// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cedar-policy/cedar-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AZ-8 says anything irreversible is named by a policy rather than left to a
// coarse group. Nothing enforced it, and the bundle did not hold to it.
//
// The action groups are derived from the HTTP method, which is the right default
// and says nothing about consequence. So deleteNodePool sat in `write` and was
// granted to every operator — and DeleteNodePoolWithTx enqueues TeardownNodePool,
// which tears down every node in the pool. Adding a destructive endpoint granted
// it to every operator with no diff to policies.cedar at all, which is the part
// that makes this a process failure rather than a policy one: there was nothing
// for a reviewer to look at.
//
// This is that missing diff. Every write operation must be named by a policy or
// listed below with all four fields of an exception — reason, blast radius,
// owner, and what would make it stop being true — a person wrote out.

// exception records one write operation AZ-8 does not reach: a case where
// operators-may-write, not a named Cedar policy, is what actually grants it.
// TestAcknowledgedWritesAreCompleteAndStillExist requires every field
// non-empty, so an entry that only argues "why" without saying what's at
// stake or who to ask fails the build, not just the review — though it can
// only check that an argument was made, not that it's true.
type exception struct {
	// The engineering argument for why the gap is acceptable, in this specific
	// case, today.
	Reason string

	// What happens if Reason turns out to be wrong — the specific bad outcome,
	// not "could be misused." For a write, what does the operation actually do.
	BlastRadius string

	// Who to ask before changing or removing this. A team or role, not a name
	// that leaves the company.
	Owner string

	// What would make this stop being true — an API change, a new Cedar
	// capability, a deprecation. "Never" is a legitimate answer, but it has to
	// be written, not implied by omission.
	Reevaluate string
}

// writesLeftToTheGroup are the write operations deliberately covered by
// operators-may-write alone. Each entry is an argument, and adding one is the
// point at which somebody has to make it.
var writesLeftToTheGroup = map[string]exception{
	"createCluster": {
		Reason: "additive: it brings an object into existence, and destroying it is deleteCluster, which is named",
		BlastRadius: "CreateCluster unconditionally queues a ProvisionCluster workflow (clusters.go:223-229), " +
			"and any node pools included in the request are created and queued for provisioning in the same " +
			"transaction (clusters.go:231-237, 468-479) — this call alone can stand up real infrastructure, not " +
			"just an unwanted database row with no teardown. The bound is that deleteCluster is named by a " +
			"policy, so tearing that infrastructure back down is not itself left to the coarse grant.",
		Owner:      "NKE control plane",
		Reevaluate: "never, unless creation itself starts doing something destructive (e.g. auto-adopting an existing name)",
	},
	"createNodePool": {
		Reason: "additive, as above; the pool's destructive edge is deleteNodePool and updateNodePool, both named",
		BlastRadius: "desiredNodeCount can be greater than zero in the request (openapi.yaml:1226-1230; nothing " +
			"forces it to zero), and CreateNodePool queues a ProvisionNodePool workflow in the same transaction " +
			"unconditionally (node_pools.go:152-163) — nodes can be provisioned by this call alone, not just an " +
			"empty pool record. The bound is the same shape as createCluster: deleteNodePool and updateNodePool, " +
			"the operations that would scale or tear the pool down, are both named.",
		Owner:      "NKE control plane",
		Reevaluate: "never, unless creation stops queuing a provisioning workflow and starts doing something narrower",
	},
	"updateCluster": {
		Reason: "one operationId covers every cluster-level PATCH field — control-plane node recycling, a " +
			"Kubernetes version upgrade, Teleport enrollment, nested control-plane config, and the two one-way " +
			"toggles enableEtcdEncryption/enableOidc (openapi.yaml:1019-1060) — and they can't be separated here " +
			"because the request body isn't visible to this middleware, which decides before the body is parsed",
		BlastRadius: "an operator with only the coarse write grant can do anything UpdateCluster allows in one " +
			"call: flip enableEtcdEncryption or enableOidc (both one-way per openapi.yaml, \"false is rejected\"), " +
			"start a control-plane Kubernetes version upgrade (clusters.go:555-577, version-skew validated but not " +
			"gated by anything narrower than write), toggle control-plane node recycling and Teleport enrollment " +
			"(clusters.go:549-554), or change the nested control-plane config (clusters.go:540-547, 605-610). " +
			"Worst case is not limited to a cluster stuck OIDC-on or encryption-on — it's an unreviewed Kubernetes " +
			"upgrade or control-plane reconfiguration on a live cluster, a materially larger blast radius than the " +
			"two one-way toggles alone.",
		Owner:      "NKE control plane",
		Reevaluate: "the day updateCluster is split into distinct operationIds per field group; that's an API change, tracked separately from this map",
	},
}

// shippedGroupOnlyWrites is groupOnlyWrites (write_group.go) over the shipped
// policies.cedar: the same function the load-time report uses, so the fence
// and the report cannot disagree about what the write group covers.
func shippedGroupOnlyWrites(t *testing.T) map[string]bool {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(bundleDir(t), policiesFile))
	require.NoError(t, err)
	policies, err := cedar.NewPolicySetFromBytes(policiesFile, source)
	require.NoError(t, err)

	// Without a permit on the write group nothing is left to it, and every
	// check below would pass vacuously.
	require.True(t, grantsWriteGroup(policies), "no permit grants NKE::Action::%q; the check would pass vacuously", writeActionGroup)
	left := map[string]bool{}
	for _, action := range groupOnlyWrites(policies) {
		left[action] = true
	}
	return left
}

func TestEveryWriteIsNamedOrAcknowledged(t *testing.T) {
	writes := 0
	for pattern := range operations {
		if !safeMethod(pattern) {
			writes++
		}
	}
	require.NotZero(t, writes, "no write operations were checked")

	for action := range shippedGroupOnlyWrites(t) {
		_, acknowledged := writesLeftToTheGroup[action]
		assert.Truef(t, acknowledged,
			"%s is a write that no policy names, so operators-may-write grants it: name it in %s, "+
				"or add it to writesLeftToTheGroup with all four exception fields filled in",
			action, policiesFile)
		// Field completeness for an acknowledged entry is checked directly
		// against the map, not from inside this loop — see
		// TestAcknowledgedWritesAreCompleteAndStillExist. This loop only
		// reaches an entry when nothing currently names the action, so
		// checking completeness here would silently stop covering an entry
		// the moment a policy also names it.
	}
}

// The list must not outlive what it describes, and every entry must argue all
// four questions, not just Reason. Checked against the registry directly,
// rather than from inside TestEveryWriteIsNamedOrAcknowledged's unnamed-write
// loop, so an entry is field-checked regardless of whether a policy also
// happens to name that action today — see the comment on that loop.
//
// This proves an argument was made for every field, not that the argument is
// true: a confidently wrong BlastRadius passes the same as a correct one.
// What it closes is the gap that let deleteNodePool sit in the write group
// unnoticed — an entry can no longer go in with a missing field.
//
// It also closes the companion gap: an exception surviving after its action
// becomes policy-named. TestEveryWriteIsNamedOrAcknowledged's loop only ever
// reaches an entry when nothing currently names the action (it ranges over
// the group-only writes, which exclude named ones), so a stale double-listed entry would pass
// there silently forever -- exactly the "no policy diff for a destructive
// grant" failure mode this whole fence exists to catch, just one step later.
// Checked here instead, against the registry directly, for the same reason
// field-completeness is.
func TestAcknowledgedWritesAreCompleteAndStillExist(t *testing.T) {
	writes := map[string]bool{}
	for pattern, op := range operations {
		if !safeMethod(pattern) {
			writes[op.Action] = true
		}
	}
	left := shippedGroupOnlyWrites(t)
	for action, exc := range writesLeftToTheGroup {
		assert.Truef(t, writes[action],
			"writesLeftToTheGroup names %q, which is not a write operation of this API", action)
		assert.Truef(t, !writes[action] || left[action],
			"writesLeftToTheGroup names %q, but a policy now names it too -- remove the stale exception entry", action)
		assert.NotEmpty(t, strings.TrimSpace(exc.Reason), action+": Reason is empty")
		assert.NotEmpty(t, strings.TrimSpace(exc.BlastRadius), action+": BlastRadius is empty")
		assert.NotEmpty(t, strings.TrimSpace(exc.Owner), action+": Owner is empty")
		assert.NotEmpty(t, strings.TrimSpace(exc.Reevaluate), action+": Reevaluate is empty")
	}
}

// What the fences do to a request is asserted where every other grant is, in
// bundleMatrix — driven through the real middleware, so it says what a
// deployment does rather than what the file appears to say.
