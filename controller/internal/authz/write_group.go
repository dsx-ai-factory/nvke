// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"log/slog"
	"slices"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
	"github.com/cedar-policy/cedar-go/x/exp/ast"
)

const (
	// writeActionGroup is the coarse action group every write operation is a
	// member of in actions.json (hack/gen-authz-actions derives it from the
	// HTTP method). A policy that grants it grants every write, including
	// writes added after the policy was written.
	writeActionGroup = "write"
)

// GroupOnlyWriteRecorder records how many write operations the loaded bundle
// grants only through the write group. A function rather than a gauge so this
// package does not depend on internal/metrics, the same shape as Recorder.
type GroupOnlyWriteRecorder func(count int)

// safeMethod reports whether a route pattern's method is one that does not
// change state. Those operations are the read group; every other operation is
// a write. hack/gen-authz-actions groups by the same rule.
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

// actionScope returns the NKE::Action IDs a policy's action scope names, and
// whether the scope is unconstrained (a bare `action`). Read from the parsed
// policy, so spacing, comments and string contents do not change the answer.
func actionScope(policy *cedar.Policy) ([]string, bool) {
	var ids []string
	add := func(uid types.EntityUID) {
		if uid.Type == ActionType {
			ids = append(ids, string(uid.ID))
		}
	}
	switch scope := policy.AST().Action.(type) {
	case ast.ScopeTypeAll:
		return nil, true
	case ast.ScopeTypeEq:
		add(scope.Entity)
	case ast.ScopeTypeIn:
		add(scope.Entity)
	case ast.ScopeTypeInSet:
		for _, uid := range scope.Entities {
			add(uid)
		}
	}
	return ids, false
}

// grantsWriteGroup reports whether some permit's action scope covers the write
// group: unconstrained, or naming NKE::Action::"write" with ==, in, or in a
// list.
func grantsWriteGroup(policies *cedar.PolicySet) bool {
	for _, policy := range policies.All() {
		if policy.AST().Effect != ast.EffectPermit {
			continue
		}
		ids, unconstrained := actionScope(policy)
		if unconstrained || slices.Contains(ids, writeActionGroup) {
			return true
		}
	}
	return false
}

// groupOnlyWrites returns, sorted, every write operation in the compiled
// operations table that no policy's action scope names, permit or forbid,
// when at least one permit grants the write group (grantsWriteGroup). Those
// operations are granted by the group alone: a write added to the API after
// the policy was written lands in this list. Empty when no permit grants the
// write group, since then no write reaches anyone through it.
//
// Only the scope is read. when and unless conditions are ignored: an action
// mentioned only in a condition is not named, and a permit whose condition
// never holds still counts as granting the group.
//
// Shared by loadBundle's report and the CI write fence
// (policy_fences_test.go), so the two agree on what the group covers.
func groupOnlyWrites(policies *cedar.PolicySet) []string {
	if !grantsWriteGroup(policies) {
		return nil
	}
	named := map[string]bool{}
	for _, policy := range policies.All() {
		ids, _ := actionScope(policy)
		for _, id := range ids {
			named[id] = true
		}
	}
	var writes []string
	for pattern, op := range operations {
		if safeMethod(pattern) || named[op.Action] {
			continue
		}
		writes = append(writes, op.Action)
	}
	slices.Sort(writes)
	return slices.Compact(writes)
}

// reportGroupOnlyWrites logs each of local's group-only writes and records
// their count. Called for the bundle loaded at startup. A nil logger or
// recorder skips that half.
func reportGroupOnlyWrites(logger *slog.Logger, record GroupOnlyWriteRecorder, local *Local) {
	if logger != nil {
		for _, action := range local.groupOnlyWrites {
			logger.Info("write operation the policy covers only through the write action group",
				"action", action,
				"group", `NKE::Action::"`+writeActionGroup+`"`,
			)
		}
	}
	if record != nil {
		record(len(local.groupOnlyWrites))
	}
}
