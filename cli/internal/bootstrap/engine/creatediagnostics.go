// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// createFailureReport is the file dumpCreateFailure writes into
// Config.DiagnosticsDir.
const createFailureReport = "bootstrap-cluster-create-failure.txt"

// controlPlaneNode returns the node container name kind gives the
// control plane of clusterName.
func controlPlaneNode(clusterName string) string {
	return clusterName + "-control-plane"
}

// dumpCreateFailure collects what a failed `kind create cluster` left inside
// the node container, into one file under cfg.DiagnosticsDir.
//
// This runs against a cluster that never reached Ready, so kubectl is not an
// option — the API server is exactly what is missing. Everything here goes
// through the container runtime instead, and reads the node's own view: the
// kubelet journal, the runtime's container and pod lists, and the logs of the
// control-plane static pods kubeadm was waiting on.
//
// Every probe is best-effort and its failure is recorded rather than
// returned. The caller already has a real error to report and must not lose
// it to a diagnostics problem, and a create can fail early enough that the
// container does not exist at all.
func (e *Engine) dumpCreateFailure(ctx context.Context, cfg Config) {
	if cfg.DiagnosticsDir == "" {
		return
	}
	if err := os.MkdirAll(cfg.DiagnosticsDir, 0o750); err != nil {
		e.log.Warn("could not create the diagnostics directory",
			"dir", cfg.DiagnosticsDir, "err", err)
		return
	}

	node := controlPlaneNode(cfg.ClusterName)
	e.log.Info("collecting diagnostics from the failed bootstrap kind cluster",
		"bootstrap_kind_cluster", cfg.ClusterName, "node", node, "dir", cfg.DiagnosticsDir)

	var report strings.Builder
	for _, probe := range createFailureProbes(node) {
		report.WriteString("### " + probe.label + "\n")
		out, err := e.Runner.Run(ctx, nil, RuntimeCLI, probe.args...)
		report.WriteString(out)
		if err != nil {
			report.WriteString("\n" + err.Error() + "\n")
		}
		report.WriteString("\n")
	}

	path := filepath.Join(cfg.DiagnosticsDir, createFailureReport)
	if err := os.WriteFile(path, []byte(report.String()), 0o600); err != nil {
		e.log.Warn("could not write the create-failure diagnostics", "file", path, "err", err)
	}
}

// probe is one command whose output goes into the create-failure report.
type probe struct {
	label string
	args  []string
}

// createFailureProbes is the ordered set of commands dumpCreateFailure runs.
// Order is the order a reader wants them: does the container exist, did it
// boot, did the kubelet come up, and what did the static pods do.
func createFailureProbes(node string) []probe {
	nodeShell := func(label, script string) probe {
		return probe{
			label: label,
			args:  []string{"exec", node, "sh", "-c", script},
		}
	}

	// crictl addresses the node's containerd directly. kubeadm's static pods
	// are not in any namespace the kubelet can be asked about without an API
	// server, so they are found by name through the runtime.
	staticPodLogs := func(name string) probe {
		return nodeShell(name+" logs", fmt.Sprintf(
			`id=$(crictl ps -a --name %s -o json 2>/dev/null | grep -o '"id": "[^"]*"' | head -1 | cut -d'"' -f4); `+
				`if [ -n "$id" ]; then crictl logs --tail 500 "$id"; else echo "no %s container"; fi`,
			name, name,
		))
	}

	return []probe{
		{label: "node containers", args: []string{"ps", "-a"}},
		// The node image declares VOLUME /var/lib/containerd, so the node's
		// containerd stores snapshots in an anonymous volume. Overlay cannot
		// be mounted with its upperdir on overlayfs, so if that volume lands
		// on the runner's own root every sandbox fails to start and nothing
		// says why. These two report the backing filesystem directly.
		{label: "volumes", args: []string{"volume", "ls"}},
		nodeShell("/var/lib/containerd backing filesystem",
			`stat -f -c %T /var/lib/containerd; findmnt -T /var/lib/containerd || true`),
		{label: node + ": boot output", args: []string{"logs", node}},
		nodeShell("kubelet journal", "journalctl -u kubelet --no-pager -n 2000"),
		nodeShell("containers", "crictl ps -a"),
		nodeShell("pods", "crictl pods"),
		nodeShell("static pod manifests", "ls -l /etc/kubernetes/manifests && cat /etc/kubernetes/manifests/*.yaml"),
		staticPodLogs("kube-apiserver"),
		staticPodLogs("etcd"),
	}
}
