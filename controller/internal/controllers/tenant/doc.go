// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
