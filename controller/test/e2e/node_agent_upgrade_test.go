// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

const agentFixtureInitial = "v0.1.991"

// agentInstallationState is the durable state written by either installation path.
type agentInstallationState struct {
	TargetVersion   string    `json:"targetVersion"`
	PreviousVersion string    `json:"previousVersion"`
	Method          string    `json:"method"`
	Phase           string    `json:"phase"`
	ErrorMessage    string    `json:"errorMessage"`
	CreatedAt       time.Time `json:"createdAt"`
}

func validateNodeAgentUpgrade(test *tenantTest) {
	It("installs the desired agent before kubeadm and authenticates fresh observations", func(ctx context.Context) {
		nodes := expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
		Expect(nodes).To(HaveLen(3))
		for _, node := range nodes {
			container := expectSucceed(nodeContainerName(ctx, node.Name))
			recorded := expectSucceed(agentDockerOutput(ctx, "exec", container, "cat", "/var/lib/nvke/e2e-kubeadm-agent-versions.log"))
			Expect(strings.Fields(recorded)).NotTo(BeEmpty())
			for _, line := range strings.Split(strings.TrimSpace(recorded), "\n") {
				Expect(line).To(ContainSubstring(agentFixtureInitial), "kubeadm must run with the lifecycle target")
			}
			state := expectSucceed(readAgentInstallation(ctx, node, agentFixtureInitial))
			Expect(state.Phase).To(Equal("Succeeded"))
			Expect(state.PreviousVersion).To(Equal("v0.1.990"))
			// The image fixture must exercise bootstrap before later native installations.
			Expect(runDockerCommand(ctx, "exec", container, "test", "-f", "/var/lib/nvke/agent-upgrades/"+agentFixtureInitial+"_bundle/updater.py")).To(Succeed())
			// A connection with no client identity must not obtain an agent response.
			probe := `import socket,ssl
s=socket.create_connection(('127.0.0.1',15005),timeout=5)
c=ssl._create_unverified_context()
try:
 t=c.wrap_socket(s,server_hostname='agent');t.sendall(b'PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n');reply=t.recv(1024)
 if reply: raise RuntimeError('unauthenticated TLS received application data')
except (ssl.SSLError,ConnectionResetError): pass
`
			Expect(runDockerCommand(ctx, "exec", container, "python3", "-c", probe)).To(Succeed())
		}
		waitForAgentConvergence(ctx, test, agentFixtureInitial, 3)
		assertWorkerAgentVersions(ctx, test)
	}, NodeTimeout(10*time.Minute))

	It("upgrades one joined control-plane agent at a time", func(ctx context.Context) {
		nodes := expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
		holdAgentInstallations(ctx, nodes)
		DeferCleanup(cleanupAgentInstallations, nodes)
		expectSucceed(test.nvke.UpdateNodeAgentVersion(ctx, test.cluster.ID, "v0.1.992"))
		seen := map[string]bool{}
		for range nodes {
			active := waitForAgentReservation(ctx, test, "v0.1.992")
			Expect(seen[active.NodeID.String()]).To(BeFalse(), "the completed node must not be selected again")
			selected := agentNodeByID(nodes, active.NodeID.String())
			Eventually(func(ctx context.Context) error {
				return assertOnlyAgentInstallationActive(ctx, nodes, selected, "v0.1.992")
			}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Succeed())
			Consistently(func(ctx context.Context) error {
				return assertOnlyAgentInstallationActive(ctx, nodes, selected, "v0.1.992")
			}).WithContext(ctx).WithTimeout(10 * time.Second).WithPolling(time.Second).Should(Succeed())
			releaseAgentInstallations(ctx, []api.Node{selected})
			Eventually(func(ctx context.Context) error {
				return assertAgentBinaryVersion(ctx, selected, "v0.1.992")
			}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(time.Second).Should(Succeed())
			seen[selected.ID.String()] = true
			Eventually(func(ctx context.Context) bool {
				current, err := test.nvke.GetCluster(ctx, test.cluster.ID)
				return err == nil && (current.NodeAgentUpgrade == nil || current.NodeAgentUpgrade.NodeID != selected.ID)
			}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(BeTrue())
		}
		waitForAgentConvergence(ctx, test, "v0.1.992", 3)
		assertWorkerAgentVersions(ctx, test)
	}, NodeTimeout(15*time.Minute))

	It("downgrades through the same controller path", func(ctx context.Context) {
		expectSucceed(test.nvke.UpdateNodeAgentVersion(ctx, test.cluster.ID, agentFixtureInitial))
		waitForAgentConvergence(ctx, test, agentFixtureInitial, 3)
		assertWorkerAgentVersions(ctx, test)
	}, NodeTimeout(10*time.Minute))

	It("restores a failed installation and retries the same node until the desired version converges", func(ctx context.Context) {
		nodes := expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
		expectSucceed(test.nvke.UpdateNodeAgentVersion(ctx, test.cluster.ID, "v0.1.993"))
		active := waitForAgentReservation(ctx, test, "v0.1.993")
		selected := agentNodeByID(nodes, active.NodeID.String())
		Eventually(func(ctx context.Context) error {
			state, err := readAgentInstallation(ctx, selected, "v0.1.993")
			if err != nil {
				return err
			}
			if state.Phase != "FailedRestored" {
				return fmt.Errorf("installation phase is %s", state.Phase)
			}
			if state.ErrorMessage == "" {
				return fmt.Errorf("failure was not recorded")
			}
			return assertAgentBinaryVersion(ctx, selected, agentFixtureInitial)
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(200 * time.Millisecond).Should(Succeed())
		Eventually(func(ctx context.Context) bool {
			cluster, err := test.nvke.GetCluster(ctx, test.cluster.ID)
			return err == nil && cluster.NodeAgentUpgrade != nil && cluster.NodeAgentUpgrade.NodeID == selected.ID && cluster.NodeAgentUpgrade.Phase == "Running" && cluster.NodeAgentUpgrade.ErrorMessage != ""
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(BeTrue())
		Consistently(func(ctx context.Context) error {
			cluster, err := test.nvke.GetCluster(ctx, test.cluster.ID)
			if err != nil {
				return err
			}
			if cluster.NodeAgentUpgrade == nil || cluster.NodeAgentUpgrade.NodeID != selected.ID {
				return fmt.Errorf("failed installation lost its selected node")
			}
			for _, node := range nodes {
				if node.ID != selected.ID {
					if err := assertAgentBinaryVersion(ctx, node, agentFixtureInitial); err != nil {
						return err
					}
				}
			}
			return nil
		}).WithContext(ctx).WithTimeout(40 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
		// Repair only this run's artifact. No cluster update or operator retry follows.
		volume := os.Getenv("NVKE_E2E_NODE_AGENT_BUNDLE_VOLUME")
		Expect(volume).NotTo(BeEmpty())
		Expect(runDockerCommand(ctx, "run", "--rm", "-v", volume+":/bundle", "alpine:3.22", "sh", "-ec", "cp /bundle/agent-e2e/repaired.tgz /bundle/agent-e2e/repaired.next; mv /bundle/agent-e2e/repaired.next /bundle/agent-e2e/agent_v0.1.993.tgz")).To(Succeed())
		waitForAgentConvergence(ctx, test, "v0.1.993", 3)
		assertWorkerAgentVersions(ctx, test)
	}, NodeTimeout(15*time.Minute))

	It("drains an accepted target across restarts and defers a new join until installation ends", func(ctx context.Context) {
		nodes := expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
		holdAgentInstallations(ctx, nodes)
		DeferCleanup(cleanupAgentInstallations, nodes)
		expectSucceed(test.nvke.UpdateNodeAgentVersion(ctx, test.cluster.ID, "v0.1.992"))
		active := waitForAgentReservation(ctx, test, "v0.1.992")
		selected := agentNodeByID(nodes, active.NodeID.String())
		Eventually(func(ctx context.Context) error {
			return assertOnlyAgentInstallationActive(ctx, nodes, selected, "v0.1.992")
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Succeed())
		original := expectSucceed(readAgentInstallation(ctx, selected, "v0.1.992"))
		container := expectSucceed(nodeContainerName(ctx, selected.Name))
		Expect(runDockerCommand(ctx, "exec", container, "systemctl", "restart", "nvke-agent.service")).To(Succeed())
		Expect(nvke.RestartNodeAgentTestController(ctx, test.k8sClient)).To(Succeed())
		expectSucceed(test.nvke.UpdateNodeAgentVersion(ctx, test.cluster.ID, agentFixtureInitial))
		expectSucceed(test.nvke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, 4))
		test.spec.ControlPlane.Config.DesiredCount = 4
		Consistently(func(ctx context.Context) error {
			current, err := test.nvke.GetCluster(ctx, test.cluster.ID)
			if err != nil {
				return err
			}
			if current.NodeAgentUpgrade == nil || current.NodeAgentUpgrade.NodeID != selected.ID || current.NodeAgentUpgrade.TargetVersion != "v0.1.992" {
				return fmt.Errorf("accepted target changed before installation completed")
			}
			state, err := readAgentInstallation(ctx, selected, "v0.1.992")
			if err != nil {
				return err
			}
			if state.CreatedAt != original.CreatedAt {
				return fmt.Errorf("restart replaced the accepted installation")
			}
			actual, err := test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)
			if err != nil {
				return err
			}
			joined := 0
			for _, node := range actual {
				if node.Conditions.Joined.Status == api.ConditionStatusTrue {
					joined++
				}
			}
			if joined != 3 {
				return fmt.Errorf("a node joined while installation was active")
			}
			return assertOnlyAgentInstallationActive(ctx, nodes, selected, "v0.1.992")
		}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(time.Second).Should(Succeed())
		releaseAgentInstallations(ctx, nodes)
		waitForAgentConvergence(ctx, test, agentFixtureInitial, 4)
		for _, node := range expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)) {
			container := expectSucceed(nodeContainerName(ctx, node.Name))
			recorded := expectSucceed(agentDockerOutput(ctx, "exec", container, "cat", "/var/lib/nvke/e2e-kubeadm-agent-versions.log"))
			Expect(recorded).To(ContainSubstring(agentFixtureInitial))
		}
		assertWorkerAgentVersions(ctx, test)
	}, NodeTimeout(20*time.Minute))
	It("drains an accepted installation before replacing a deleted control-plane node", func(ctx context.Context) {
		nodes := expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
		holdAgentInstallations(ctx, nodes)
		DeferCleanup(cleanupAgentInstallations, nodes)
		expectSucceed(test.nvke.UpdateNodeAgentVersion(ctx, test.cluster.ID, "v0.1.992"))
		active := waitForAgentReservation(ctx, test, "v0.1.992")
		selected := agentNodeByID(nodes, active.NodeID.String())
		Eventually(func(ctx context.Context) error {
			return assertOnlyAgentInstallationActive(ctx, nodes, selected, "v0.1.992")
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(test.nvke.DeleteNode(ctx, selected.ID)).To(Succeed())
		Consistently(func(ctx context.Context) error {
			actual, err := test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)
			if err != nil {
				return err
			}
			found := false
			for _, node := range actual {
				if node.ID == selected.ID {
					found = true
					if node.Conditions.Deleting.Status == "True" {
						return fmt.Errorf("node teardown started before the accepted installation drained")
					}
				}
			}
			if !found {
				return fmt.Errorf("selected node was removed before installation ended")
			}
			return assertOnlyAgentInstallationActive(ctx, nodes, selected, "v0.1.992")
		}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(time.Second).Should(Succeed())
		releaseAgentInstallations(ctx, nodes)
		Eventually(func(ctx context.Context) bool {
			actual, err := test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)
			if err != nil {
				return false
			}
			for _, node := range actual {
				if node.ID == selected.ID {
					return false
				}
			}
			return len(actual) == 4
		}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(2 * time.Second).Should(BeTrue())
		waitForAgentConvergence(ctx, test, "v0.1.992", 4)
		assertWorkerAgentVersions(ctx, test)
	}, NodeTimeout(20*time.Minute))
}

func agentDockerOutput(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", commandError(command, err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func assertAgentBinaryVersion(ctx context.Context, node api.Node, version string) error {
	container, err := nodeContainerName(ctx, node.Name)
	if err != nil {
		return err
	}
	output, err := agentDockerOutput(ctx, "exec", container, "/usr/local/bin/nvke-agent", "--version")
	if err != nil {
		return err
	}
	fields := strings.Fields(output)
	if len(fields) == 0 || fields[len(fields)-1] != version {
		return fmt.Errorf("node %s agent version: %s; want %s", node.Name, output, version)
	}
	return nil
}

func readAgentInstallation(ctx context.Context, node api.Node, target string) (agentInstallationState, error) {
	container, err := nodeContainerName(ctx, node.Name)
	if err != nil {
		return agentInstallationState{}, err
	}
	output, err := agentDockerOutput(ctx, "exec", container, "cat", "/var/lib/nvke/agent-upgrades/"+target+"_bundle/operation.json")
	if err != nil {
		return agentInstallationState{}, err
	}
	var state agentInstallationState
	err = json.Unmarshal([]byte(output), &state)
	return state, err
}

func agentNodeByID(nodes []api.Node, id string) api.Node {
	for _, node := range nodes {
		if node.ID.String() == id {
			return node
		}
	}
	Fail("selected node is absent: " + id)
	return api.Node{}
}

func waitForAgentReservation(ctx context.Context, test *tenantTest, target string) api.NodeAgentUpgradeStatus {
	var state api.NodeAgentUpgradeStatus
	Eventually(func(ctx context.Context) bool {
		cluster, err := test.nvke.GetCluster(ctx, test.cluster.ID)
		if err != nil || cluster.NodeAgentUpgrade == nil || cluster.NodeAgentUpgrade.TargetVersion != target {
			return false
		}
		state = *cluster.NodeAgentUpgrade
		return true
	}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(BeTrue())
	return state
}

func waitForAgentConvergence(ctx context.Context, test *tenantTest, target string, count int) {
	Eventually(func(ctx context.Context) error {
		cluster, err := test.nvke.GetCluster(ctx, test.cluster.ID)
		if err != nil {
			return err
		}
		if cluster.NodeAgentUpgrade != nil {
			return fmt.Errorf("agent convergence is active: %+v", cluster.NodeAgentUpgrade)
		}
		nodes, err := test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)
		if err != nil {
			return err
		}
		if len(nodes) != count {
			return fmt.Errorf("control-plane count is %d, want %d", len(nodes), count)
		}
		for _, node := range nodes {
			if err := assertAgentBinaryVersion(ctx, node, target); err != nil {
				return err
			}
		}
		return test.assertDesiredNodesReady(ctx)
	}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}

func assertWorkerAgentVersions(ctx context.Context, test *tenantTest) {
	for _, node := range expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, false)) {
		Expect(assertAgentBinaryVersion(ctx, node, "v0.1.990")).To(Succeed())
	}
}

func holdAgentInstallations(ctx context.Context, nodes []api.Node) {
	for _, node := range nodes {
		container := expectSucceed(nodeContainerName(ctx, node.Name))
		Expect(runDockerCommand(ctx, "exec", container, "sh", "-ec", `mkdir -p /etc/systemd/system/nvke-agent-updater@.service.d; touch /var/lib/nvke/e2e-agent-hold; printf '%s\n' '[Service]' "ExecStartPre=/bin/sh -c 'while [ -f /var/lib/nvke/e2e-agent-hold ]; do sleep 1; done'" >/etc/systemd/system/nvke-agent-updater@.service.d/qualification.conf; systemctl daemon-reload`)).To(Succeed())
	}
}

func releaseAgentInstallations(ctx context.Context, nodes []api.Node) {
	for _, node := range nodes {
		container, err := nodeContainerName(ctx, node.Name)
		if err != nil {
			continue
		}
		Expect(runDockerCommand(ctx, "exec", container, "rm", "-f", "/var/lib/nvke/e2e-agent-hold")).To(Succeed())
	}
}

func cleanupAgentInstallations(ctx context.Context, nodes []api.Node) {
	for _, node := range nodes {
		container, err := nodeContainerName(ctx, node.Name)
		if err != nil {
			continue
		}
		if err := runDockerCommand(ctx, "exec", container, "rm", "-f", "/var/lib/nvke/e2e-agent-hold"); err != nil {
			GinkgoLogr.Info("releasing agent hold during cleanup failed", "node", node.Name, "error", err)
		}
	}
}

func assertOnlyAgentInstallationActive(ctx context.Context, nodes []api.Node, selected api.Node, target string) error {
	active := 0
	for _, node := range nodes {
		container, err := nodeContainerName(ctx, node.Name)
		if err != nil {
			return err
		}
		output, err := agentDockerOutput(ctx, "exec", container, "systemctl", "show", "--property=ActiveState", "--value", "nvke-agent-updater@"+target+"_bundle.service")
		if err != nil {
			return err
		}
		if output == "activating" || output == "active" || output == "deactivating" {
			active++
			if node.ID != selected.ID {
				return fmt.Errorf("another node started an installation: %s", node.Name)
			}
		}
	}
	if active != 1 {
		return fmt.Errorf("active installations: %d, want exactly one", active)
	}
	return nil
}

func deleteClusterWithAgentDrain(ctx context.Context, test *tenantTest, accepted *bool) {
	nodes := expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
	holdAgentInstallations(ctx, nodes)
	DeferCleanup(cleanupAgentInstallations, nodes)
	expectSucceed(test.nvke.UpdateNodeAgentVersion(ctx, test.cluster.ID, agentFixtureInitial))
	active := waitForAgentReservation(ctx, test, agentFixtureInitial)
	selected := agentNodeByID(nodes, active.NodeID.String())
	Eventually(func(ctx context.Context) error {
		return assertOnlyAgentInstallationActive(ctx, nodes, selected, agentFixtureInitial)
	}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Succeed())
	Expect(test.lifecycle.DeleteCluster(ctx, test.cluster.ID)).To(Succeed())
	*accepted = true
	Consistently(func(ctx context.Context) error {
		for _, node := range nodes {
			if _, err := nodeContainerName(ctx, node.Name); err != nil {
				return fmt.Errorf("cluster deletion removed a node before draining installation: %w", err)
			}
		}
		return assertOnlyAgentInstallationActive(ctx, nodes, selected, agentFixtureInitial)
	}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(time.Second).Should(Succeed())
	// Lose the host while its installer is held. Cluster deletion must finish
	// without an agent response or completion of the accepted installation.
	container := expectSucceed(nodeContainerName(ctx, selected.Name))
	Expect(runDockerCommand(ctx, "stop", "--time", "0", container)).To(Succeed())
	Expect(test.lifecycle.WaitForClusterDeleted(ctx, test.cluster.ID)).To(Succeed())
}
