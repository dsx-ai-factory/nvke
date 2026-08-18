// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/NVIDIA/nke/controller/internal/db"
)

// This is a faithful port of SWE's KubeAPIHealthy cluster preflight
// (site-workflow-engine internal/rollout/preflights/kube_api.go + lib.go). It probes
// kube-apiserver /readyz?verbose on every control-plane node — vian agent's
// ExecuteScript RPC running `kubectl --raw` against that node's OWN apiserver
// (--server https://localhost:6443) — and gates on healthyRequired(N) nodes being
// healthy. Running the probe on each node (rather than a single network call to the
// VIP) is what lets it detect an individual apiserver that is down while the VIP is
// still served by a peer.

const (
	kubeReadyzPath                = "/readyz?verbose"
	preflightKubeReadyzScriptName = "preflight-kube-readyz"
)

// probeControlPlaneAPIHealth probes /readyz on every control-plane node and returns an
// error unless at least healthyRequired(len(nodes)) are healthy.
func (a *Activities) probeControlPlaneAPIHealth(ctx context.Context, nodes []db.Node) error {
	n := len(nodes)
	if n == 0 {
		return fmt.Errorf("kube-apiserver health: no control-plane nodes to probe")
	}
	threshold := healthyRequired(n)
	healthy := 0
	failed := make([]string, 0, n)
	for _, node := range nodes {
		detail, ok := a.probeKubeReadyz(ctx, node)
		if ok {
			healthy++
			continue
		}
		failed = append(failed, fmt.Sprintf("%s (%s)", node.Name, detail))
	}
	if healthy < threshold {
		return fmt.Errorf(
			"kube-apiserver health: %d/%d control-plane nodes healthy (need >= %d for safe rollout); failing=[%s]",
			healthy, n, threshold, strings.Join(failed, ", "),
		)
	}
	return nil
}

// probeKubeReadyz runs the /readyz?verbose probe on one control-plane node. It returns
// (detail, healthy); indeterminate output fails closed.
func (a *Activities) probeKubeReadyz(ctx context.Context, node db.Node) (string, bool) {
	resp, err := a.nodeAgentClient.ExecuteScript(ctx, node, preflightKubeReadyzScriptName, kubectlRawLocal(kubeReadyzPath), nil)
	if err != nil {
		return fmt.Sprintf("exec err: %v", err), false
	}
	failing, sawAny := parseReadyzVerbose(resp.GetOutput())
	if resp.GetExitCode() != 0 {
		// --raw bypasses kubectl error formatting, so failing checks can still appear
		// in stdout when the exit code is non-zero.
		if len(failing) > 0 {
			return fmt.Sprintf("failing checks: %v", failing), false
		}
		return fmt.Sprintf("exit=%d stderr=%q", resp.GetExitCode(), strings.TrimSpace(resp.GetErrorMessage())), false
	}
	if !sawAny {
		return fmt.Sprintf("no readyz markers in %d-byte response", len(resp.GetOutput())), false
	}
	if len(failing) > 0 {
		return fmt.Sprintf("failing checks: %v", failing), false
	}
	return "", true
}

// kubectlRawLocal returns a `kubectl --raw <path>` script pinned to localhost so the
// probe targets this node's own apiserver, not a peer. The script carries its own
// shebang so the agent's direct-exec path actually runs it (a bare script
// ENOEXECs); the path is shell-quoted so metacharacters like '?' aren't glob-expanded.
func kubectlRawLocal(path string) string {
	return "#!/bin/sh\n" +
		"exec kubectl --kubeconfig /etc/kubernetes/admin.conf " +
		"--server https://localhost:6443 --insecure-skip-tls-verify=true " +
		"get --raw " + shellQuote(path) + "\n"
}

// shellQuote single-quotes s so shell metacharacters (e.g. '?') aren't expanded when
// the path is interpolated into the kubectl script above.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseReadyzVerbose extracts failing check names from `/readyz?verbose`. ok=false when
// no [+]/[-] marker is seen (garbled stream — fail closed).
func parseReadyzVerbose(stdout string) ([]string, bool) {
	var failing []string
	sawAny := false
	for raw := range strings.SplitSeq(stdout, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "[+]"):
			sawAny = true
		case strings.HasPrefix(line, "[-]"):
			sawAny = true
			name := strings.TrimPrefix(line, "[-]")
			// "[-]etcd failed: …" → "etcd"
			if i := strings.IndexAny(name, " \t"); i > 0 {
				name = name[:i]
			}
			failing = append(failing, name)
		}
	}
	return failing, sawAny
}

// healthyRequired = ceil((N+1)/2) + 1, capped at N. Quorum size plus one headroom so
// the rollout refuses to start if losing exactly one healthy CP mid-roll would drop
// quorum. N=1:1, N=2:2, N=3:3, N=4:4, N=5:4, N=7:5.
func healthyRequired(n int) int {
	if n <= 0 {
		return 0
	}
	quorum := (n + 1 + 1) / 2 // ceil((n+1)/2)
	return min(quorum+1, n)
}
