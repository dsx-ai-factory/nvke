// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// The NKE API policy bundle: the hand-written half. schema.json and actions.json
// beside this file are generated from api/openapi.yaml by
// hack/gen-authz-actions, roles.json declares the roles, and roleMappings.json
// grants them.
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

// Node lifecycle is destructive and is held to one role.
//
// The permit alone does not achieve that. The action groups are generated from
// the HTTP method, so every one of these is already in `write` — and the
// operator rule above would grant it. Carving an operation back out of a coarse
// group takes a forbid, which in Cedar beats every permit. `unless` is what
// leaves the one role able to do it.
//
// The general rule this is an instance of: a method-derived group is the right
// default and the wrong answer for anything destructive. Those get named.
//
// deleteNodePool is here for a reason the endpoint's name does not carry:
// DeleteNodePoolWithTx enqueues TeardownNodePool, which tears down every node in
// the pool. It read as one object's deletion and was granted to every operator.
// updateNodePool is the same shape without the word "delete" in it — a profile
// change rolls the pool's nodes.
//
// policy_fences_test.go is what keeps this list honest: a write operation added
// to the API has to be named here or acknowledged there, by a person, in a
// sentence.
@id("node-operators-may-change-node-lifecycle")
permit(
  principal in NKE::Role::"nodeOperator",
  action in [
    NKE::Action::"deleteNode",
    NKE::Action::"deleteNodePool",
    NKE::Action::"updateNodePool"
  ],
  resource
);

@id("only-node-operators-change-node-lifecycle")
forbid(
  principal,
  action in [
    NKE::Action::"deleteNode",
    NKE::Action::"deleteNodePool",
    NKE::Action::"updateNodePool"
  ],
  resource
)
unless { principal in NKE::Role::"nodeOperator" };

// Cluster deletion is destructive and named explicitly, the same reasoning
// node lifecycle gets a named rule for: operators-may-write's coarse "write"
// grant already reaches deleteCluster (it is a DELETE, so it is in the
// group), but leaving it implicit there is the shape AZ-8 exists to close.
//
// Two separate facts, worth not collapsing into one: this permit is redundant
// with operators-may-write — it names nothing that grant does not already
// cover — so by itself it changes nothing about who can do what. But the
// change this permit shipped as part of did change who could delete clusters:
// it replaced a standing nobody-deletes-clusters forbid, which had made
// deleteCluster unreachable for everyone regardless of the coarse write grant.
// Removing that forbid, not adding this permit, is what let operators start
// deleting clusters; this permit only makes the resulting grant a line a
// reviewer can find, instead of an inference from the group.
@id("operators-may-delete-clusters")
permit(
  principal in NKE::Role::"operator",
  action == NKE::Action::"deleteCluster",
  resource
);

// Repointing a cluster's SPIRE server is named for the same reason. It is a
// PATCH, so operators-may-write already grants it, and it changes which server
// every node's SPIRE agent takes its identity from. This permit changes nobody's
// access; it makes the grant a line a reviewer can find.
@id("operators-may-repoint-spire-servers")
permit(
  principal in NKE::Role::"operator",
  action == NKE::Action::"updateClusterSpireServer",
  resource
);
