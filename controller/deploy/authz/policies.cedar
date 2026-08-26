// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// The NKE API policy bundle: the hand-written half. schema.json and actions.json
// beside this file are generated from api/openapi.yaml by
// hack/gen-authz-actions, and roles.json names who holds which role.
//
// Default deny. Nothing here is a fallback: a request matching no permit is
// refused, and that includes an operation added to the API after this file was
// last edited.

// Readers may perform any read operation. The rule names the group rather than
// the operations, so a read endpoint added later is covered without editing
// this file — the generated actions.json puts it in the group.
@id("readers-may-read")
permit(
  principal in NKE::Role::"reader",
  action in NKE::Action::"read",
  resource
);

// Operators may write. Deliberately not "may do anything": an unconstrained
// `action` would also grant every operation added after this was written, which
// is the one direction where a new endpoint fails open.
@id("operators-may-write")
permit(
  principal in NKE::Role::"operator",
  action in NKE::Action::"write",
  resource
);

// Node deletion is destructive and irreversible, so it is held to one role.
//
// The permit alone does not achieve that. The action groups are generated from
// the HTTP method, so deleteNode is a DELETE and therefore already in `write` —
// and the operator rule above would grant it. Carving an operation back out of a
// coarse group takes a forbid, which in Cedar beats every permit. `unless` is
// what leaves the one role able to do it.
//
// The general rule this is an instance of: a method-derived group is the right
// default and the wrong answer for anything irreversible. Those get named.
@id("node-operators-may-delete-nodes")
permit(
  principal in NKE::Role::"nodeOperator",
  action == NKE::Action::"deleteNode",
  resource
);

@id("only-node-operators-delete-nodes")
forbid(
  principal,
  action == NKE::Action::"deleteNode",
  resource
)
unless { principal in NKE::Role::"nodeOperator" };

// Cluster deletion is granted to nobody by this bundle. Removing the account
// that could do it is not the same as there being no rule; there is no rule.
@id("nobody-deletes-clusters")
forbid(
  principal,
  action == NKE::Action::"deleteCluster",
  resource
);
