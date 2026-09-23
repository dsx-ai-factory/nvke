// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package preflights

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/nodeagent"
)

// execScriptOnNode dials the agent and runs script via the raw
// ExecuteScript RPC. err = gRPC failure; exitCode=0 = script success.
func execScriptOnNode(
	ctx context.Context,
	dialer nodeagent.Dialer,
	node rollout.Node,
	scriptName, script string,
) (int32, string, string, error) {
	if dialer == nil {
		return 0, "", "", errors.New("preflights: dialer not configured")
	}
	client, cleanup, err := dialer(ctx, node)
	if err != nil {
		return 0, "", "", fmt.Errorf("dial %s: %w", node.Name(), err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	resp, err := client.ExecuteScript(ctx, &nkeproto.ExecuteScriptRequest{
		ScriptName:     scriptName,
		ScriptContents: script,
	})
	if err != nil {
		return 0, "", "", fmt.Errorf("execute %s on %s: %w", scriptName, node.Name(), err)
	}
	return resp.GetExitCode(), resp.GetOutput(), resp.GetErrorMessage(), nil
}

// dialAndStatus is the shared SPIFFE-dial + GetKubeadmStatus kernel
// for the NodeAgent preflights (reachability, readiness, version).
func dialAndStatus(
	ctx context.Context,
	dialer nodeagent.Dialer,
	node rollout.Node,
) (*nkeproto.KubeadmStatusResponse, error) {
	if dialer == nil {
		return nil, errors.New("preflights: dialer not configured")
	}
	client, cleanup, err := dialer(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", node.Name(), err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	resp, err := client.GetKubeadmStatus(ctx, &nkeproto.KubeadmStatusRequest{})
	if err != nil {
		return nil, fmt.Errorf("get_kubeadm_status on %s: %w", node.Name(), err)
	}
	return resp, nil
}

// kubectlRawLocal returns a `kubectl --raw <path>` script pinned to
// localhost so the probe targets this CP's own apiserver, not a peer.
// The script carries its own shebang so the agent's direct-exec
// path actually runs it (a bare script ENOEXECs); the path is
// shell-quoted so metacharacters like '?' aren't glob-expanded.
func kubectlRawLocal(path string) string {
	return "#!/bin/sh\n" +
		"exec kubectl --kubeconfig /etc/kubernetes/admin.conf " +
		"--server https://localhost:6443 --insecure-skip-tls-verify=true " +
		"get --raw " + shellQuote(path) + "\n"
}

// shellQuote single-quotes s so shell metacharacters (e.g. '?') aren't
// expanded when the path is interpolated into the kubectl script above.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseReadyzVerbose extracts failing check names from `/readyz?verbose`.
// ok=false when no [+]/[-] marker seen (garbled stream — fail-closed).
func parseReadyzVerbose(stdout string) ([]string, bool) {
	var failing []string
	sawAny := false
	for _, raw := range strings.Split(stdout, "\n") {
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

// versionAtLeast compares MAJOR.MINOR.PATCH cores; any "-suffix" /
// "+meta" trailer is stripped (so "v0.1.17-3-gf5bc014" == "v0.1.17").
// Malformed input returns false (fail-closed).
//
// The suffix-strip is deliberate: the agent build pipeline emits
// git-describe tags like "v0.1.17-3-gf5bc014" for every CI build past
// a tag, and strict semver would reject all of them. Cost: rc tags
// like "v0.1.21-rc.1" are also treated as >= "v0.1.21" — pin to the
// next release if you need to gate a wire-incompatible change.
func versionAtLeast(actual, minimum string) bool {
	a, aOk := parseSemverCore(actual)
	m, mOk := parseSemverCore(minimum)
	if !aOk || !mOk {
		return false
	}
	for i := range a {
		if a[i] != m[i] {
			return a[i] > m[i]
		}
	}
	return true
}

// parseSemverCore extracts [major, minor, patch] from a version string.
func parseSemverCore(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return [3]int{}, false
	}
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	patch, errPatch := strconv.Atoi(parts[2])
	if errMajor != nil || errMinor != nil || errPatch != nil || major < 0 || minor < 0 || patch < 0 {
		return [3]int{}, false
	}
	return [3]int{major, minor, patch}, true
}

// healthyRequired = ceil((N+1)/2) + 1, capped at N. Quorum size plus
// one headroom so the rollout refuses to start if losing exactly one
// healthy CP mid-roll would drop quorum. N=1:1, N=2:2, N=3:3, N=4:4,
// N=5:4, N=7:5.
func healthyRequired(n int) int {
	if n <= 0 {
		return 0
	}
	quorum := (n + 1 + 1) / 2 // ceil((n+1)/2)
	threshold := quorum + 1
	if threshold > n {
		threshold = n
	}
	return threshold
}
