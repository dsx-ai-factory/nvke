// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"os"
	"path/filepath"
	"testing"

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
// listed below with a reason a person wrote.

// writesLeftToTheGroup are the write operations deliberately covered by
// operators-may-write alone. Each entry is an argument, and adding one is the
// point at which somebody has to make it.
var writesLeftToTheGroup = map[string]string{
	"createCluster":  "additive: it brings an object into existence, and destroying it is deleteCluster, which is named",
	"createNodePool": "additive, as above; the pool's destructive edge is deleteNodePool and updateNodePool, both named",

	// The honest limit of a gate outside the mux, and the reason AZ-8 is scoped
	// rather than absolute. One PATCH carries a cosmetic rename and
	// enableEtcdEncryption / enableOidc, which openapi.yaml documents as one-way
	// ("false is rejected because disabling etcd encryption is not supported").
	// They are one operationId, so they are one Cedar action, and no rule here
	// can separate them: the request body is not in the query and cannot be,
	// because this middleware decides before the body is parsed. Splitting them
	// is an API change — a distinct operation per one-way door — not a policy
	// one.
	"updateCluster": "one operationId covers a cosmetic PATCH and two one-way doors; separating them needs an API change, not a policy",
}

func TestEveryWriteIsNamedOrAcknowledged(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(bundleDir(t), policiesFile))
	require.NoError(t, err)

	named := map[string]bool{}
	for _, m := range actionRef.FindAllStringSubmatch(string(source), -1) {
		named[m[1]] = true
	}
	require.NotEmpty(t, named, "no policy names an action; the check would pass vacuously")

	writes := 0
	for pattern, op := range operations {
		if safeMethod(pattern) {
			continue
		}
		writes++
		if named[op.Action] {
			continue
		}
		reason, acknowledged := writesLeftToTheGroup[op.Action]
		assert.Truef(t, acknowledged,
			"%s is a write that no policy names, so operators-may-write grants it: name it in %s, "+
				"or add it to writesLeftToTheGroup with the reason that is acceptable",
			op.Action, policiesFile)
		assert.NotEmptyf(t, reason, "%s is acknowledged with no reason", op.Action)
	}
	require.NotZero(t, writes, "no write operations were checked")
}

// The list must not outlive what it describes. An entry for an operation the API
// no longer has is an argument nobody will re-read, and it hides the next one.
func TestEveryAcknowledgedWriteStillExists(t *testing.T) {
	writes := map[string]bool{}
	for pattern, op := range operations {
		if !safeMethod(pattern) {
			writes[op.Action] = true
		}
	}
	for action := range writesLeftToTheGroup {
		assert.Truef(t, writes[action],
			"writesLeftToTheGroup names %q, which is not a write operation of this API", action)
	}
}

// What the fences do to a request is asserted where every other grant is, in
// bundleMatrix — driven through the real middleware, so it says what a
// deployment does rather than what the file appears to say.

func safeMethod(pattern string) bool {
	method, _, ok := splitPattern(pattern)
	if !ok {
		return false
	}
	switch method {
	case "GET", "HEAD", "OPTIONS", "TRACE":
		return true
	default:
		return false
	}
}
