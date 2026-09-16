// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// The "readOnly" named preset (design-authz-chart-validation.md §4): the
// starter bundle apiAuthz.policyMode: readOnly selects. Grants only the
// reader role's permissions -- nobody holds operator or nodeOperator in this
// preset's matching roles.json, so this document's only rule is the whole
// grant.
//
// Kept as a subset of deploy/authz/policies.cedar's "readers-may-read" rule
// rather than a hand-derived rewrite, so the two cannot drift on what "may
// read" means.
//
// Default deny. Nothing here is a fallback: a request matching no permit is
// refused, and that includes an operation added to the API after this file
// was last edited.

@id("readers-may-read")
permit(
  principal in NKE::Role::"reader",
  action in NKE::Action::"read",
  resource
);
