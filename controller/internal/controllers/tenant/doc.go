// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tenant manages one controller-runtime manager for each tenant
// cluster that CWE is currently able and allowed to manage.
//
// The management-cluster Secret informer is the access-material event source.
// It does not communicate with managerRuntime directly:
//
//	cluster creation
//	     |
//	     v
//	clusters row is live                 TenantAccessController
//	     |                                      |
//	     |                              publishes kubeconfig Secret
//	     |                              labeled with cluster UUID
//	     |                                      |
//	     +--------------------------------------v
//	                                    Secret LIST/WATCH
//	                                           |
//	                                           v
//	                                    SecretReconciler
//	                                GET Secret + GET clusters row
//	                                           |
//	                       +-------------------+-------------------+
//	                       |                                       |
//	                live row + access                  missing/invalid access
//	                       |                              or deleting row
//	                       v                                       v
//	               ManagerSet.Upsert                 ManagerSet.Remove*
//	                       |                                       |
//	                       v                                       |
//	    map[cluster UUID]*tenantManagerGeneration <---------------+
//	                       |
//	                       v
//	               ManagerFactory.Start
//	                       |
//	                       v
//	          controller-runtime Manager connected
//	               to the tenant kube-apiserver
//	                       |
//	                       v
//	           managerRuntime{cancel, done}
//	                       |
//	                       v
//	               tenant CSR controller
//	             LISTs and WATCHes CSRs
//
// ManagerSet receives a notification because SecretReconciler calls Upsert,
// RemoveKubeconfigSecret, or RemoveCluster directly. ManagerSet does not watch
// Kubernetes itself. It owns the per-cluster tenantManagerGeneration,
// serializes kubeconfig rotation, limits concurrent starts, and retries one
// cluster without blocking the rest.
//
// managerRuntime is only a lifecycle handle around the controller-runtime
// Manager started by ManagerFactory:
//
//   - cancel stops that manager and all tenant controllers registered in it;
//   - done reports when manager.Start has returned.
//
// ManagerSet retains this handle so it can stop the old manager and wait for
// done before starting a replacement for the same cluster UUID.
//
// Per-cluster event flow
//
//	Secret available + clusters row live
//	                  |
//	                  v
//	             start manager
//	                  |
//	                  v
//	               RUNNING
//	                  |
//	                  +-- same Secret UID and kubeconfig -----> no change
//	                  |
//	                  +-- Secret recreated or kubeconfig changes
//	                  |                                      -> stop old manager
//	                  |                                         wait for exit
//	                  |                                         start replacement
//	                  |                                         return to RUNNING
//	                  |
//	                  +-- Secret deleted or cluster terminal -> stop manager
//	                  |                                         do not restart
//	                  |
//	                  +-- manager exits unexpectedly ---------> retry start
//	                                                            return to RUNNING
//
// A failed initial or replacement start is retried with backoff. CWE shutdown
// stops every manager and does not restart them.
//
// # Scaling, limitations, and future direction
//
// The initial implementation keeps all tenant control loops in one CWE process.
// ManagerSet creates one controller-runtime Manager for every active tenant
// cluster. Each manager has its own REST client, cache, CSR informer, work queue,
// and controller goroutines connected to that tenant cluster.
//
// The startup semaphore limits how many managers connect concurrently; it does
// not limit the number of running managers. Client QPS and burst settings limit
// requests to each tenant API server, but aggregate connections, goroutines, and
// cache memory still grow linearly with the number of tenant clusters. The
// initial sizing target is 20 active tenant managers; this is not an enforced
// capacity limit or an HA guarantee. Per-manager metrics and health listeners
// are currently disabled.
//
// Increasing the CWE Deployment replica count is not a supported way to scale
// this subsystem. Tenant-manager leader election and distributed ownership are
// disabled, so every CWE replica would observe the same access Secrets and
// start another manager for every tenant cluster, duplicating watches and
// reconciliation.
//
// Future scale-out requires assigning each tenant cluster to exactly one active
// controller owner. Two candidate models are:
//
//   - Per-cluster workload: create a dedicated pod for each tenant cluster. The
//     pod runs one manager and provides strong failure isolation and independent
//     resource limits, at the cost of one Kubernetes workload per cluster and
//     additional scheduling, rollout, and garbage-collection machinery.
//   - Sharded, multiplexed workers: assign disjoint sets of tenant clusters to a
//     bounded pool of controller pods. Cluster-specific watch sources enqueue a
//     typed key containing both cluster UUID and object identity into a shared
//     work queue, and the reconciler selects the corresponding tenant client.
//     This uses fewer pods and provides shared backpressure, but requires
//     distributed ownership, dynamic source lifecycle management, and careful
//     isolation of failures between clusters.
//
// A controller-runtime Manager's normal cache and client are bound to one
// rest.Config. Multiplexing therefore cannot make one ordinary cache watch
// several tenant clusters: it still needs a distinct client and watch/cache
// source for each cluster. It reduces manager and worker duplication, but does
// not remove the linear number of tenant API-server watches.
//
// Either scale-out model must add ownership fencing, failure reassignment,
// per-cluster observability, and capacity testing before the single-process
// assumption can be removed.
//
// Bootstrap ordering
//
//	initial control-plane kube-vip and kubeadm initialization complete
//	                              |
//	                              v
//	        commit the control-plane node's Joined=True condition
//	                              |
//	                              v
//	       DB node source enqueues the owning cluster UUID
//	                              |
//	                              v
//	          fetch admin.conf from the initial control-plane node
//	                              |
//	                              v
//	       apply tenant RBAC when OIDC is enabled, then publish Secret
//	                              |
//	                              v
//	        SecretReconciler starts the tenant controller manager
//	                              |
//	                              v
//	       CSR informer queues existing and newly observed pending CSRs
//	                              |
//	                              v
//	          validate each CSR against CWE node identity and approve
//
// Tenant-access publication is triggered by the committed Joined=True update;
// it does not wait for the whole cluster to become Provisioned. The CSR
// informer queues existing pending CSRs during cache sync and newly observed
// pending CSRs from its watch.
//
// When the durable cluster row enters Deleting, the same cluster source stops
// the tenant manager immediately. Temporal retains the access Secrets until its
// final teardown step, then removes them after deleting the cluster row.
package tenant
