// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package preflights provides the reusable safety floor every CWE-driven
// rollout operation should run before mutating a cluster. The checks are
// agent-flavored — they reach the cluster over a nodeagent.Dialer
// (the CWE binding is adapters/cwe.NewSPIFFEDialer) and run kubectl locally
// on each CP rather than from the worker directly. They depend only on the
// rollout core and the nodeagent transport contract, not on the CWE data
// model.
//
// # What's here
//
// Three families, four checks:
//
//   - NodeAgentReachable (cluster, default-on) — proves the SPIFFE-mTLS
//     dial succeeds and the agent answers GetKubeadmStatus on every CP.
//     See DefaultNodePreflights for why this lives at cluster scope.
//   - NodeAgentReady     (node, opt-in)     — adds status == "ready"
//     to catch a node mid-kubeadm-phase.
//   - NodeAgentVersion   (node, opt-in)     — gates on a minimum
//     agent version supplied via the ref's Params["min_version"].
//   - KubeAPIHealthy     (cluster, default-on) — runs
//     `kubectl --raw /readyz?verbose` against every CP's local apiserver
//     and requires `ceil((N+1)/2) + 1` of them healthy, so the rollout
//     keeps a safe majority of control planes available while it takes
//     one down.
//
// # Control-plane topology assumption
//
// These defaults assume etcd runs stacked on each control plane — every
// apiserver talks to its co-located etcd over localhost — which is how
// NKE deploys today. The apiserver's /readyz includes an etcd readiness
// gate, so the per-CP KubeAPIHealthy check already covers etcd health;
// no separate etcd probe is needed, and the rollout worker does not require
// direct etcd access (which it does not have). A control plane with external or
// non-co-located etcd would need a dedicated etcd check with direct
// access — add one here if that topology is adopted.
//
// # How they get installed
//
// The worker (internal/temporal/register.go) calls RegisterDefaults(dialer)
// once at startup, passing the SPIFFE-aware dialer it constructed.
// After that, each preflight is available under its canonical name
// (e.g. "cwe.kube-api-healthy") and any RolloutSpec can opt in by
// reference.
//
// The temporal workflow factory injects the default set
// (DefaultClusterPreflights, DefaultNodePreflights) into every spec
// it builds unless the consumer's WorkflowConfig sets
// SuppressDefaultPreflights = true. The escape hatch exists for
// operations that must run against a degraded cluster — for example
// remediation that has to proceed even when the API server is
// unhealthy or a control-plane majority is unavailable; no finer-grain
// per-preflight opt-out is supported — when you suppress the defaults
// you take ownership of supplying whatever subset you want.
//
// # Why "kubectl on the CP" instead of an in-process Kubernetes client
//
// CWE has no direct TLS material for tenant apiservers. Existing
// activities either talk to the Site Controller's OWN apiserver via the
// worker's in-cluster ServiceAccount, or fetch the tenant kubeconfig via
// the agent (GetKubeconfig) and use it for narrow CA extraction.
// None of them dial a tenant apiserver directly from the worker.
//
// The /readyz family is local-only ("localhost:6443") which makes it
// the right place to ask "is this CP's apiserver healthy" without
// needing the CWE worker to hold tenant TLS material. Reuses the same
// dialer + ExecuteScript path rollout operations already use, so
// there's no new auth path to manage.
package preflights
