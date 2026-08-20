// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/substrate"
)

const (
	// Paths kubeadm creates on init and removes on reset. Teardown specs read
	// them off the host, because the API reporting a node gone says nothing
	// about whether the machine was left usable.
	kubeadmAdminConfPath = "/etc/kubernetes/admin.conf"
	kubeadmManifestsPath = "/etc/kubernetes/manifests"
	kubeVIPManifestPath  = kubeadmManifestsPath + "/kube-vip.yaml"

	// Paths CWE writes only when SPIRE is in play. capiSpireAttestationMethod
	// "none" must produce neither, and the assertion is on the bootstrap secret
	// rather than the host: the secret is what CWE decided to send.
	spireAgentConfigPath = "/etc/spire-agent/agent.conf"
	spireTrustBundlePath = "/etc/spire-agent/initial-trust-bundle.pem"

	preprovisionedGroupVersion = "infrastructure.cluster.x-k8s.io/v1alpha1"
	capiGroupVersion           = "cluster.x-k8s.io/v1beta2"
)

// Exercises the preprovisioned generic-CAPI path against hosts that already
// exist. The substrate stands the hosts up as containers; from CWE's side they
// are simply machines it was told about, and nothing on this path may create
// infrastructure.
//
// The specs are ordered as rungs: each one assumes the rung below it passed, so
// a failure names the first thing that broke rather than the last.
var _ = Describe("Preprovisioned hosts", Ordered, ContinueOnFailure,
	Label("preprovisioned"), func() {
		var (
			sub  *substrate.Substrate
			test tenantTest
		)

		BeforeAll(func(ctx context.Context) {
			test = expectSucceed(setupTenantTest(ctx, nke.Options{
				Provision: 40 * time.Minute,
				Delete:    15 * time.Minute,
				Poll:      10 * time.Second,
			}))

			By("Starting the preprovisioned host substrate")
			started, err := substrate.Start(ctx, substrate.DefaultConfig(), func(format string, args ...any) {
				GinkgoWriter.Printf(format+"\n", args...)
			})
			Expect(err).NotTo(HaveOccurred(), "bringing up the substrate")
			sub = started

			DeferCleanup(func(ctx context.Context) { sub.Stop(ctx) })
		}, NodeTimeout(15*time.Minute))

		// Rung 0: the substrate itself is sound. This validates the part most
		// likely to cost a day of debugging — containers that boot systemd, run
		// nke-agent, and are reachable at an address docker chose.
		It("stands up hosts that look already-provisioned to CWE", func(ctx context.Context) {
			nodes := sub.Nodes()
			Expect(nodes).To(HaveLen(3), "the multi-control-plane acceptance test needs three hosts")

			seen := map[string]string{}
			for _, node := range nodes {
				By(fmt.Sprintf("Verifying host %s", node.Name))
				Expect(node.IP).NotTo(BeEmpty(),
					"host %s should have an address discovered from docker", node.Name)

				running, err := substrate.Running(ctx, node.Container)
				Expect(err).NotTo(HaveOccurred())
				Expect(running).To(BeTrue(), "container %s should be running", node.Container)

				Expect(seen).NotTo(HaveKey(node.IP),
					"hosts %s and %s share address %s", seen[node.IP], node.Name, node.IP)
				seen[node.IP] = node.Name
			}
		}, Label("substrate"), NodeTimeout(2*time.Minute))

		// Rung 1: CWE accepts the inventory inside capiInfraCluster and the
		// provider reports the cluster provisioned. Nothing is bound yet.
		//
		// Reporting provisioned is not cosmetic: the core Cluster API machine
		// controller waits for it before it acts on any machine, so a cluster
		// stuck here stalls every node.
		It("accepts an inventory in capiInfraCluster and reports the cluster provisioned",
			func(ctx context.Context) {
				cluster := createPreprovisionedCluster(ctx, test, sub)

				By("Verifying the API stored the inventory it was given")
				stored := expectSucceed(test.nke.GetCluster(ctx, cluster.ID))
				hosts := inventoryFromManifest(stored.CapiInfraCluster)
				Expect(hosts).To(HaveLen(len(sub.Nodes())))
				for _, node := range sub.Nodes() {
					Expect(hosts).To(HaveKeyWithValue(node.Name, node.IP),
						"CWE must store the address it was given, not one it discovered")
				}

				By("Waiting for the provider to validate the inventory")
				namespace := clusterNamespace(cluster.Name)
				Eventually(func(g Gomega) {
					infraCluster := getUnstructured(ctx, g, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedCluster", namespace, cluster.Name)

					provisioned, found, err := unstructured.NestedBool(
						infraCluster.Object, "status", "initialization", "provisioned")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(found).To(BeTrue(), "the provider should report initialization")
					g.Expect(provisioned).To(BeTrue())

					// Not "all hosts are free": the machine reconciler binds
					// within a second of the cluster being applied, so there is
					// no window in which that is observably true. What holds at
					// every instant is that the two counts partition the
					// inventory.
					available, found, err := unstructured.NestedInt64(
						infraCluster.Object, "status", "availableHosts")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(found).To(BeTrue(),
						"availableHosts must be reported even when it is zero")

					allocated, _, err := unstructured.NestedSlice(
						infraCluster.Object, "status", "allocatedHosts")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(int(available)+len(allocated)).To(Equal(len(sub.Nodes())),
						"free and held hosts should account for the whole inventory")
				}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

				By("Verifying the CAPI Cluster points at the preprovisioned infrastructure")
				capiCluster := &unstructured.Unstructured{}
				capiCluster.SetGroupVersionKind(mustParseGVK(capiGroupVersion, "Cluster"))
				Expect(test.k8sClient.Get(ctx,
					ctrlclient.ObjectKey{Namespace: namespace, Name: cluster.Name}, capiCluster)).To(Succeed())
				kind, _, _ := unstructured.NestedString(capiCluster.Object, "spec", "infrastructureRef", "kind")
				Expect(kind).To(Equal("PreprovisionedCluster"))
			}, Label("api"), NodeTimeout(10*time.Minute))

		// Rung 2: every machine binds to a distinct host and publishes its
		// address, and nothing creates infrastructure while doing so.
		It("binds one machine per host without creating infrastructure",
			func(ctx context.Context) {
				hosts := sub.Nodes()

				containersBefore, err := substrate.ContainerCount(ctx)
				Expect(err).NotTo(HaveOccurred())

				cluster := createPreprovisionedCluster(ctx, test, sub)
				namespace := clusterNamespace(cluster.Name)

				By("Waiting for a bound infrastructure machine per host")
				var boundHosts map[string]string
				Eventually(func(g Gomega) {
					machines := listUnstructured(ctx, g, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedMachineList", namespace)
					g.Expect(machines.Items).To(HaveLen(len(hosts)))

					boundHosts = map[string]string{}
					for _, machine := range machines.Items {
						providerID, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
						g.Expect(providerID).To(HavePrefix("preprovisioned://"+cluster.Name+"/"),
							"machine %s should be bound to a host of this cluster", machine.GetName())

						provisioned, found, _ := unstructured.NestedBool(
							machine.Object, "status", "initialization", "provisioned")
						g.Expect(found).To(BeTrue(), "machine %s should report initialization", machine.GetName())
						g.Expect(provisioned).To(BeTrue())

						hostName, _, _ := unstructured.NestedString(machine.Object, "status", "hostName")
						g.Expect(hostName).NotTo(BeEmpty())
						g.Expect(boundHosts).NotTo(HaveKey(hostName),
							"host %s was given to two machines", hostName)
						boundHosts[hostName] = machine.GetName()
					}
				}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

				By("Verifying each machine publishes the address of the host it holds")
				addressByHost := map[string]string{}
				for _, host := range hosts {
					addressByHost[host.Name] = host.IP
				}
				for hostName, machineName := range boundHosts {
					Expect(addressByHost).To(HaveKey(hostName),
						"machine %s holds host %q, which is not in the inventory", machineName, hostName)

					machine := getUnstructured(ctx, Default, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedMachine", namespace, machineName)
					addresses, _, _ := unstructured.NestedSlice(machine.Object, "status", "addresses")
					Expect(addresses).NotTo(BeEmpty(), "machine %s should publish an address", machineName)
					first, _ := addresses[0].(map[string]any)
					Expect(first["address"]).To(Equal(addressByHost[hostName]))
				}

				// Cluster API copies spec.providerID from the infrastructure
				// machine onto the Machine. The provider-ID controller depends
				// on that copy, so assert it rather than assume it.
				By("Verifying Cluster API copied the provider IDs onto the Machines")
				Eventually(func(g Gomega) {
					machines := listUnstructured(ctx, g, test.k8sClient, capiGroupVersion, "MachineList", namespace)
					g.Expect(machines.Items).To(HaveLen(len(hosts)))
					for _, machine := range machines.Items {
						providerID, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
						g.Expect(providerID).To(HavePrefix("preprovisioned://"+cluster.Name+"/"),
							"Machine %s should carry the provider ID of its infrastructure machine", machine.GetName())
					}
				}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

				By("Verifying CWE recorded the inventory addresses on its own node rows")
				Eventually(func(g Gomega) {
					cweNodes, err := test.nke.ListClusterNodes(ctx, cluster.ID)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(cweNodes).To(HaveLen(len(hosts)))

					addresses := map[string]struct{}{}
					for _, node := range cweNodes {
						g.Expect(node.IP).NotTo(BeEmpty(), "node %s should have an address", node.Name)
						addresses[node.IP] = struct{}{}
					}
					g.Expect(addresses).To(HaveLen(len(hosts)), "two nodes share an address")
					for _, host := range hosts {
						g.Expect(addresses).To(HaveKey(host.IP),
							"no node took the address of host %s", host.Name)
					}
				}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

				By("Verifying the cluster reports the allocations")
				infraCluster := getUnstructured(ctx, Default, test.k8sClient,
					preprovisionedGroupVersion, "PreprovisionedCluster", namespace, cluster.Name)
				allocated, _, _ := unstructured.NestedSlice(infraCluster.Object, "status", "allocatedHosts")
				Expect(allocated).To(HaveLen(len(hosts)))
				available, found, _ := unstructured.NestedInt64(infraCluster.Object, "status", "availableHosts")
				Expect(found).To(BeTrue(), "an exhausted inventory must still report availableHosts")
				Expect(available).To(BeNumerically("==", 0))

				// The whole point of this path: no infrastructure was created.
				By("Verifying nothing created infrastructure")
				containersAfter, err := substrate.ContainerCount(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(containersAfter).To(Equal(containersBefore),
					"the preprovisioned path must not create or destroy hosts")

				dockerMachines := emptyList("infrastructure.cluster.x-k8s.io/v1beta2", "DockerMachineList")
				if err := test.k8sClient.List(ctx, dockerMachines, ctrlclient.InNamespace(namespace)); err == nil {
					Expect(dockerMachines.Items).To(BeEmpty(),
						"nothing on this path should create infrastructure machines")
				}
			}, Label("api", "lifecycle"), NodeTimeout(20*time.Minute))

		// Rung 3: capiSpireAttestationMethod "none", end to end. A
		// preprovisioned host booted long before CWE learned it existed, so no
		// cloud-init ran and no SPIRE material could reach it. CWE must send
		// none, and must not have tried to read a trust bundle to get there.
		It("writes bootstrap data with no SPIRE material when attestation is none",
			func(ctx context.Context) {
				cluster := createPreprovisionedCluster(ctx, test, sub)
				namespace := clusterNamespace(cluster.Name)

				Eventually(func(g Gomega) {
					cweNodes, err := test.nke.ListClusterNodes(ctx, cluster.ID)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(cweNodes).To(HaveLen(len(sub.Nodes())))

					for _, node := range cweNodes {
						secret := &corev1.Secret{}
						g.Expect(test.k8sClient.Get(ctx, ctrlclient.ObjectKey{
							Namespace: namespace,
							Name:      node.Name + "-bootstrap-data",
						}, secret)).To(Succeed())

						g.Expect(secret.Data).To(HaveKey("value"))
						userData := string(secret.Data["value"])
						g.Expect(userData).To(HavePrefix("#cloud-config"))
						g.Expect(userData).NotTo(ContainSubstring(spireAgentConfigPath),
							"attestation none must write no SPIRE agent config")
						g.Expect(userData).NotTo(ContainSubstring(spireTrustBundlePath),
							"attestation none must write no SPIRE trust bundle")
					}
				}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
			}, Label("api", "spire"), NodeTimeout(10*time.Minute))

		// Rung 4: the real thing. CWE drives kubeadm over the plaintext
		// node-agent channel on machines it never provisioned.
		It("builds a three-node Kubernetes control plane on the preprovisioned hosts",
			func(ctx context.Context) {
				hosts := sub.Nodes()
				cluster := createPreprovisionedCluster(ctx, test, sub)

				By("Waiting for the cluster to finish provisioning")
				_, err := test.nke.WaitForClusterReady(ctx, cluster.ID)
				Expect(err).NotTo(HaveOccurred(), "cluster should reach Provisioned")

				pools := expectSucceed(test.nke.ListNodePools(ctx, cluster.ID))
				Expect(pools).To(HaveLen(1))
				Expect(test.nke.WaitForProvisionedNodeCount(ctx, pools[0].ID, len(hosts))).To(Succeed())

				// The strongest assertion available: the machines the operator
				// listed are now Kubernetes nodes. Their *names* come from CWE,
				// not from the inventory, so the addresses are what tie a
				// Kubernetes node back to the host it runs on.
				By("Verifying the tenant cluster sees every host as a Kubernetes node")
				tenantClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, cluster.Name))
				Eventually(func(g Gomega) {
					kubeNodes := &corev1.NodeList{}
					g.Expect(tenantClient.List(ctx, kubeNodes)).To(Succeed())
					g.Expect(kubeNodes.Items).To(HaveLen(len(hosts)))

					addresses := map[string]string{}
					for _, kubeNode := range kubeNodes.Items {
						g.Expect(nodeIsReady(kubeNode)).To(BeTrue(), "node %s should be Ready", kubeNode.Name)
						addresses[internalIP(kubeNode)] = kubeNode.Name
					}
					for _, host := range hosts {
						g.Expect(addresses).To(HaveKey(host.IP),
							"no Kubernetes node runs on host %s (%s)", host.Name, host.IP)
					}
				}).WithTimeout(15 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

				By("Verifying Cluster API linked each Machine to its Node")
				namespace := clusterNamespace(cluster.Name)
				Eventually(func(g Gomega) {
					machines := listUnstructured(ctx, g, test.k8sClient, capiGroupVersion, "MachineList", namespace)
					g.Expect(machines.Items).To(HaveLen(len(hosts)))
					for _, machine := range machines.Items {
						nodeName, found, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
						g.Expect(found).To(BeTrue(),
							"Machine %s has no nodeRef; the provider ID never reached the tenant Node",
							machine.GetName())
						g.Expect(nodeName).NotTo(BeEmpty())
					}
				}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

				By("Verifying the host containers were never replaced")
				for _, host := range hosts {
					running, err := substrate.Running(ctx, host.Container)
					Expect(err).NotTo(HaveOccurred())
					Expect(running).To(BeTrue(),
						"container %s should be the same machine the provider bound", host.Container)

					// Only when a VIP was asked for. CWE skips kube-vip
					// entirely without one, and the substrate withholds the VIP
					// where its BGP peer is unreachable.
					if sub.ControlPlaneVIP() == "" {
						continue
					}
					configured, err := substrate.FileExists(ctx, host.Container, kubeVIPManifestPath)
					Expect(err).NotTo(HaveOccurred())
					Expect(configured).To(BeTrue(), "kube-vip should be configured on %s", host.Name)
				}
			}, Label("api", "lifecycle", "happy-path"), NodeTimeout(50*time.Minute))

		// Rung 5: teardown returns the machine, it does not consume it. This is
		// the behaviour most likely to be wrong and most expensive to get wrong
		// on real hardware: a provider-backed teardown deletes an instance, and
		// doing that to a machine CWE does not own destroys an operator's
		// server.
		It("releases the hosts on teardown instead of destroying them",
			func(ctx context.Context) {
				hosts := sub.Nodes()
				cluster := createPreprovisionedClusterNoCleanup(ctx, test, sub)

				By("Waiting for the cluster to finish provisioning")
				_, err := test.nke.WaitForClusterReady(ctx, cluster.ID)
				Expect(err).NotTo(HaveOccurred())
				pools := expectSucceed(test.nke.ListNodePools(ctx, cluster.ID))
				Expect(pools).To(HaveLen(1))
				Expect(test.nke.WaitForProvisionedNodeCount(ctx, pools[0].ID, len(hosts))).To(Succeed())

				// Proves the hosts really were carrying a control plane, so the
				// post-teardown assertions are meaningful rather than vacuous.
				for _, host := range hosts {
					exists, err := substrate.FileExists(ctx, host.Container, kubeadmAdminConfPath)
					Expect(err).NotTo(HaveOccurred())
					Expect(exists).To(BeTrue(),
						"expected %s to be running a control plane before teardown", host.Name)
				}

				By("Deleting the cluster")
				Expect(test.nke.DeleteCluster(ctx, cluster.ID)).To(Succeed())
				Expect(test.nke.WaitForClusterDeleted(ctx, cluster.ID)).To(Succeed())

				By("Verifying the machines survived")
				for _, host := range hosts {
					running, err := substrate.Running(ctx, host.Container)
					Expect(err).NotTo(HaveOccurred())
					Expect(running).To(BeTrue(),
						"teardown destroyed host %s, which CWE does not own", host.Name)
				}

				By("Verifying each host was reset rather than left mid-cluster")
				for _, host := range hosts {
					// kubeadm reset removes the admin kubeconfig and the static
					// pod manifests. A host still carrying them is not reusable:
					// the next kubeadm init would fail on a dirty machine.
					resetPaths := []string{
						kubeadmAdminConfPath,
						kubeadmManifestsPath + "/kube-apiserver.yaml",
					}
					if sub.ControlPlaneVIP() != "" {
						resetPaths = append(resetPaths, kubeVIPManifestPath)
					}
					for _, path := range resetPaths {
						exists, err := substrate.FileExists(ctx, host.Container, path)
						Expect(err).NotTo(HaveOccurred())
						Expect(exists).To(BeFalse(),
							"%s still exists on %s; the host was not reset and cannot be reused",
							path, host.Name)
					}
				}

				By("Verifying the bound infrastructure machines were removed")
				namespace := clusterNamespace(cluster.Name)
				Eventually(func(g Gomega) {
					machines := emptyList(preprovisionedGroupVersion, "PreprovisionedMachineList")
					if err := test.k8sClient.List(ctx, machines, ctrlclient.InNamespace(namespace)); err == nil {
						g.Expect(machines.Items).To(BeEmpty())
					}
				}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

				// A released host that cannot be used again has not really been
				// released. This is the assertion that makes the rung mean
				// something on real hardware.
				By("Creating a second cluster on the released hosts")
				reused := createPreprovisionedCluster(ctx, test, sub)
				_, err = test.nke.WaitForClusterReady(ctx, reused.ID)
				Expect(err).NotTo(HaveOccurred(), "the released hosts must support a second kubeadm init")
			}, Label("api", "lifecycle", "teardown"), NodeTimeout(60*time.Minute))

		// An inventory that cannot satisfy the pool is an operator mistake that
		// must be legible. The surplus machine has to say so and wait, not take
		// a host that another machine holds.
		It("reports a machine that has no free host instead of double-booking one",
			func(ctx context.Context) {
				hosts := sub.Nodes()

				spec := preprovisionedSpec(sub)
				spec.ControlPlane.Pools[0].DesiredNodeCount = len(hosts) + 1
				cluster, err := test.nke.CreateCluster(ctx, spec)
				Expect(err).NotTo(HaveOccurred())
				deferDeleteCluster(test, cluster)

				namespace := clusterNamespace(cluster.Name)
				Eventually(func(g Gomega) {
					machines := listUnstructured(ctx, g, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedMachineList", namespace)
					g.Expect(machines.Items).To(HaveLen(len(hosts) + 1))

					bound, unsatisfied := 0, 0
					for _, machine := range machines.Items {
						providerID, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
						if providerID != "" {
							bound++
							continue
						}
						unsatisfied++
						g.Expect(readyConditionReason(machine)).To(Equal("NoHostAvailable"),
							"machine %s holds no host and should say why", machine.GetName())
					}
					g.Expect(bound).To(Equal(len(hosts)), "every host should still be used")
					g.Expect(unsatisfied).To(Equal(1))
				}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
			}, Label("api", "negative"), NodeTimeout(20*time.Minute))

		// The reason PreprovisionedHostClaim exists. Two clusters, each with its
		// own inventory in its own namespace, both listing the same physical
		// machines. Without a lock visible across that boundary both would bind
		// and both would run kubeadm on the same hosts, and the second would
		// destroy the first.
		It("stops a second cluster from taking hosts the first one holds",
			func(ctx context.Context) {
				hosts := sub.Nodes()

				first := createPreprovisionedCluster(ctx, test, sub)
				By("Waiting for the first cluster to lock every host")
				Eventually(func(g Gomega) {
					claims := listUnstructured(ctx, g, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedHostClaimList", "")
					held := 0
					for _, claim := range claims.Items {
						clusterName, _, _ := unstructured.NestedString(claim.Object, "spec", "clusterName")
						if clusterName == first.Name {
							held++
						}
					}
					// One claim per address of each host. The substrate gives
					// each host a single address and no machineID.
					g.Expect(held).To(Equal(len(hosts)),
						"the first cluster should hold a claim per host")
				}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

				By("Creating a second cluster over the same hosts")
				second, err := test.nke.CreateCluster(ctx, preprovisionedSpec(sub))
				Expect(err).NotTo(HaveOccurred())
				deferDeleteCluster(test, second)

				By("Verifying the second cluster is refused every host")
				secondNamespace := clusterNamespace(second.Name)
				Eventually(func(g Gomega) {
					machines := listUnstructured(ctx, g, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedMachineList", secondNamespace)
					g.Expect(machines.Items).To(HaveLen(len(hosts)))
					for _, machine := range machines.Items {
						providerID, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
						g.Expect(providerID).To(BeEmpty(),
							"machine %s took a host the first cluster holds", machine.GetName())
						g.Expect(readyConditionReason(machine)).To(Equal("HostsClaimedElsewhere"),
							"machine %s should say why it is waiting", machine.GetName())
					}
				}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

				By("Verifying the first cluster still holds its hosts")
				firstNamespace := clusterNamespace(first.Name)
				machines := listUnstructured(ctx, Default, test.k8sClient,
					preprovisionedGroupVersion, "PreprovisionedMachineList", firstNamespace)
				for _, machine := range machines.Items {
					providerID, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
					Expect(providerID).To(HavePrefix("preprovisioned://"+first.Name+"/"),
						"the first cluster lost host %s to the second", machine.GetName())
				}
			}, Label("api", "negative", "claims"), NodeTimeout(30*time.Minute))

		// The lock must not outlive its holder, or a released host could never
		// be reused. Rung 5 covers reuse by the same cluster; this covers the
		// handover to a different one.
		It("frees the hosts for another cluster once the holder is deleted",
			func(ctx context.Context) {
				hosts := sub.Nodes()

				first := createPreprovisionedClusterNoCleanup(ctx, test, sub)
				Eventually(func(g Gomega) {
					claims := listUnstructured(ctx, g, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedHostClaimList", "")
					g.Expect(claims.Items).NotTo(BeEmpty())
				}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

				By("Deleting the first cluster")
				Expect(test.nke.DeleteCluster(ctx, first.ID)).To(Succeed())
				Expect(test.nke.WaitForClusterDeleted(ctx, first.ID)).To(Succeed())

				By("Verifying every claim was released")
				Eventually(func(g Gomega) {
					claims := listUnstructured(ctx, g, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedHostClaimList", "")
					for _, claim := range claims.Items {
						clusterName, _, _ := unstructured.NestedString(claim.Object, "spec", "clusterName")
						g.Expect(clusterName).NotTo(Equal(first.Name),
							"claim %s outlived its cluster", claim.GetName())
					}
				}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

				By("Verifying a different cluster can now take the hosts")
				second := createPreprovisionedCluster(ctx, test, sub)
				secondNamespace := clusterNamespace(second.Name)
				Eventually(func(g Gomega) {
					machines := listUnstructured(ctx, g, test.k8sClient,
						preprovisionedGroupVersion, "PreprovisionedMachineList", secondNamespace)
					g.Expect(machines.Items).To(HaveLen(len(hosts)))
					for _, machine := range machines.Items {
						providerID, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
						g.Expect(providerID).To(HavePrefix("preprovisioned://"+second.Name+"/"),
							"machine %s did not get a released host", machine.GetName())
					}
				}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
			}, Label("api", "lifecycle", "claims"), NodeTimeout(40*time.Minute))

		// The generic-CAPI validation rules are cheap to exercise and easy to
		// regress, and each one prevents a failure that would otherwise surface
		// much later and much less legibly.
		//
		// Only cluster-level rules are here. A malformed node pool inside a
		// create-cluster request does not currently reach the caller as a 400
		// (see the note on createInitialNodePools), so asserting a status code
		// for those belongs with the node-pool endpoint, not here.
		DescribeTable("rejects a malformed generic-CAPI request",
			func(ctx context.Context, mutate func(*api.CreateClusterRequest), wantMessage string) {
				req := expectSucceed(nke.BuildCreateRequest(preprovisionedSpec(sub)))
				mutate(&req)

				resp, err := test.nke.API().CreateClusterWithResponse(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode()).To(Equal(http.StatusBadRequest),
					"expected rejection, got %d: %s", resp.StatusCode(), resp.Body)
				Expect(string(resp.Body)).To(ContainSubstring(wantMessage))
			},
			Entry("without an infra cluster", func(req *api.CreateClusterRequest) {
				req.CapiInfraCluster = nil
			}, "capiInfraCluster is required when siteID is the nil UUID"),
			Entry("with an orgID", func(req *api.CreateClusterRequest) {
				req.OrgID = "some-org"
			}, "orgID must be omitted for CAPI-backed clusters"),
			Entry("with an infra cluster outside the infrastructure group",
				func(req *api.CreateClusterRequest) {
					req.CapiInfraCluster["apiVersion"] = "apps/v1"
					req.CapiInfraCluster["kind"] = "Deployment"
				}, "must be a CAPI infrastructure provider group"),
			Entry("with an infra cluster missing its kind", func(req *api.CreateClusterRequest) {
				delete(req.CapiInfraCluster, "kind")
			}, "kind is required"),
		)
	})

// preprovisionedSpec describes a single control-plane pool backed by every host
// of the substrate.
//
// capiInfraMachineTemplate carries an empty spec on purpose: there is nothing
// for an operator to configure on a machine that already exists.
func preprovisionedSpec(sub *substrate.Substrate) nke.ClusterSpec {
	hosts := make([]nke.PreprovisionedHost, 0, len(sub.Nodes()))
	for _, node := range sub.Nodes() {
		hosts = append(hosts, nke.PreprovisionedHost{Name: node.Name, IP: node.IP})
	}

	return nke.ClusterSpec{
		Name:                "preprov",
		Backend:             nke.BackendPreprovisioned,
		KubernetesVersion:   defaultKubernetesVersion,
		CNIPlugin:           string(CNIPluginCalico),
		PreprovisionedHosts: hosts,
		ControlPlane: nke.ControlPlane{
			ControlPlaneVIP: sub.ControlPlaneVIP(),
			Pools: []nke.NodePoolSpec{{
				Name:                 "cp",
				NodeNamePrefix:       "cp",
				ControlPlane:         true,
				DesiredNodeCount:     len(hosts),
				SystemReservedMemory: "1Gi",
			}},
		},
	}
}

// createPreprovisionedCluster creates a cluster and registers its deletion.
func createPreprovisionedCluster(ctx context.Context, test tenantTest, sub *substrate.Substrate) *api.Cluster {
	GinkgoHelper()
	cluster := createPreprovisionedClusterNoCleanup(ctx, test, sub)
	deferDeleteCluster(test, cluster)
	return cluster
}

// createPreprovisionedClusterNoCleanup is for the teardown spec, which deletes
// the cluster itself and then asserts on what deletion left behind.
func createPreprovisionedClusterNoCleanup(
	ctx context.Context,
	test tenantTest,
	sub *substrate.Substrate,
) *api.Cluster {
	GinkgoHelper()
	cluster, err := test.nke.CreateCluster(ctx, preprovisionedSpec(sub))
	Expect(err).NotTo(HaveOccurred(), "creating the preprovisioned cluster")
	GinkgoWriter.Printf("created cluster %s (%s) on %d preprovisioned host(s)\n",
		cluster.Name, cluster.ID, len(sub.Nodes()))
	return cluster
}

func deferDeleteCluster(test tenantTest, cluster *api.Cluster) {
	GinkgoHelper()
	DeferCleanup(func(ctx context.Context) {
		Expect(test.nke.DeleteCluster(ctx, cluster.ID)).To(Succeed())
		Expect(test.nke.WaitForClusterDeleted(ctx, cluster.ID)).To(Succeed())
	})
}

func clusterNamespace(clusterName string) string { return "cluster-" + clusterName }

// inventoryFromManifest reads host name to address out of the stored
// capiInfraCluster, so a spec can assert what CWE persisted rather than what it
// was sent.
func inventoryFromManifest(manifest map[string]any) map[string]string {
	GinkgoHelper()
	hosts, found, err := unstructured.NestedSlice(manifest, "spec", "hosts")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue(), "the stored infra cluster should carry an inventory")

	out := map[string]string{}
	for _, entry := range hosts {
		host, ok := entry.(map[string]any)
		Expect(ok).To(BeTrue())
		name, _ := host["name"].(string)
		addresses, _ := host["addresses"].([]any)
		Expect(addresses).NotTo(BeEmpty(), "host %s should carry an address", name)
		first, _ := addresses[0].(map[string]any)
		address, _ := first["address"].(string)
		out[name] = address
	}
	return out
}

func emptyList(apiVersion, kind string) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mustParseGVK(apiVersion, kind))
	return list
}

func mustParseGVK(apiVersion, kind string) schema.GroupVersionKind {
	GinkgoHelper()
	groupVersion, err := schema.ParseGroupVersion(apiVersion)
	Expect(err).NotTo(HaveOccurred())
	return groupVersion.WithKind(kind)
}

func listUnstructured(
	ctx context.Context,
	g Gomega,
	k8sClient ctrlclient.Client,
	apiVersion, kind, namespace string,
) *unstructured.UnstructuredList {
	list := emptyList(apiVersion, kind)
	g.Expect(k8sClient.List(ctx, list, ctrlclient.InNamespace(namespace))).To(Succeed())
	return list
}

func getUnstructured(
	ctx context.Context,
	g Gomega,
	k8sClient ctrlclient.Client,
	apiVersion, kind, namespace, name string,
) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(mustParseGVK(apiVersion, kind))
	g.Expect(k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: name}, obj)).To(Succeed())
	return obj
}

// readyConditionReason returns the reason of the Ready condition, which is how
// the provider explains itself when it cannot bind a machine.
func readyConditionReason(obj unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if conditionType, _ := condition["type"].(string); conditionType != "Ready" {
			continue
		}
		reason, _ := condition["reason"].(string)
		return reason
	}
	return ""
}

// nodeIsReady reports whether the kubelet has posted a Ready condition.
func nodeIsReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// internalIP returns the node's internal address, which is what ties a
// Kubernetes node back to the host it runs on.
func internalIP(node corev1.Node) string {
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			return strings.TrimSpace(address.Address)
		}
	}
	return ""
}
