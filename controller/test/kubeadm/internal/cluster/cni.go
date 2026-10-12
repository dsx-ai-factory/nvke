//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DefaultCalicoVersion is the Calico stream to install. It tracks the stream
// NVKE's own cni-packages builder currently ships, so the test exercises a
// version NVKE actually uses -- even though the artifacts here come from
// upstream rather than from NVKE's packaging.
const DefaultCalicoVersion = "v3.31.4"

// calicoManifestBase is the upstream manifest root. Public and anonymous,
// which is the whole point: NVKE's CNI packages sit behind a source-IP allowlist
// that CI runners are outside of.
const calicoManifestBase = "https://raw.githubusercontent.com/projectcalico/calico"

// upstreamPoolCIDR is the pod CIDR upstream hardcodes in custom-resources.yaml.
// It has to be replaced with the cluster's real pod network, or Calico hands
// out pod IPs from a range kubeadm knows nothing about. Nodes still report
// Ready in that state, so the readiness assertion cannot see it -- it would
// surface much later as traffic that does not route.
const upstreamPoolCIDR = "192.168.0.0/16"

// InstallCalico installs Calico by applying the upstream Tigera operator
// manifests, and returns the script output.
//
// It runs on the control-plane node over the agent's ExecuteScript RPC rather
// than from the test host, because the guests already have kubectl and
// /etc/kubernetes/admin.conf. Doing it host-side would mean installing a
// matching kubectl on the runner for no benefit.
//
// This deliberately does NOT go through the agent's InstallCNI RPC. That RPC
// requires an NVKE-named Debian package registered with dpkg
// (agent/internal/agent/cni.go, chartsDebPresenceCheck), and those packages are
// not publicly available -- driving it would mean fabricating a stub package
// impersonating one. Applying the manifests directly is honest about what is
// under test: this proves the cluster the agent built becomes usable, not that
// NVKE's CNI packaging works. The latter stays with the GitLab suite.
func InstallCalico(ctx context.Context, addr, version, podCIDR string, timeout time.Duration) (string, error) {
	if version == "" {
		version = DefaultCalicoVersion
	}
	if podCIDR == "" {
		return "", fmt.Errorf("cluster.InstallCalico: podCIDR is required")
	}

	operatorURL := fmt.Sprintf("%s/%s/manifests/tigera-operator.yaml", calicoManifestBase, version)
	resourcesURL := fmt.Sprintf("%s/%s/manifests/custom-resources.yaml", calicoManifestBase, version)

	// The CRD wait is the part worth explaining. tigera-operator.yaml declares
	// the operator Deployment but NOT the CRDs -- the operator pod creates them
	// once it is running. Applying custom-resources.yaml straight afterwards
	// fails with "no matches for kind Installation", and
	// `kubectl wait --for=condition=Established` on a resource that does not
	// exist yet errors immediately rather than waiting, because its --timeout
	// governs the condition, not the resource appearing. So: wait for create,
	// then wait for Established. Under emulation the operator needs minutes to
	// pull its image and start, hence the budgets.
	script := "#!/bin/bash\n" +
		"set -euo pipefail\n" +
		"export KUBECONFIG=/etc/kubernetes/admin.conf\n" +
		"OPERATOR_URL=" + shellQuote(operatorURL) + "\n" +
		"RESOURCES_URL=" + shellQuote(resourcesURL) + "\n" +
		"POD_CIDR=" + shellQuote(podCIDR) + "\n" +
		"UPSTREAM_CIDR=" + shellQuote(upstreamPoolCIDR) + "\n" +
		`
echo "=== fetching upstream Calico manifests ==="
curl -fsSL --retry 3 --retry-delay 5 --max-time 120 -o /tmp/tigera-operator.yaml "$OPERATOR_URL"
curl -fsSL --retry 3 --retry-delay 5 --max-time 120 -o /tmp/custom-resources.yaml "$RESOURCES_URL"

echo "=== pointing the default IPPool at the cluster pod CIDR ==="
# Match the literal only, not the indentation: it sits at a different depth in
# different Calico streams.
if grep -qF "cidr: ${UPSTREAM_CIDR}" /tmp/custom-resources.yaml; then
  sed -i "s|cidr: ${UPSTREAM_CIDR}|cidr: ${POD_CIDR}|g" /tmp/custom-resources.yaml
  echo "rewrote ${UPSTREAM_CIDR} -> ${POD_CIDR}"
else
  # Loud, and fatal. Silently applying upstream's CIDR would leave Calico
  # handing out pod IPs the cluster does not route, with every node still
  # reporting Ready -- a green run that proves nothing.
  echo "ERROR: expected 'cidr: ${UPSTREAM_CIDR}' in custom-resources.yaml; upstream layout changed" >&2
  grep -n 'cidr:' /tmp/custom-resources.yaml >&2 || true
  exit 1
fi

echo "=== applying the Tigera operator ==="
kubectl apply --server-side --force-conflicts -f /tmp/tigera-operator.yaml

echo "=== waiting for the Installation CRD to appear and become Established ==="
kubectl wait --for=create crd/installations.operator.tigera.io --timeout=600s
# --for=create can return before .status exists, and "wait --for=condition="
# then errors on a nil .status.conditions instead of waiting for it:
# "accessor error: <nil> is of the type <nil>, expected []interface{}".
for _ in $(seq 60); do
  [ -n "$(kubectl get crd installations.operator.tigera.io \
      -o jsonpath='{.status.conditions}' 2>/dev/null)" ] && break
  sleep 5
done
kubectl wait --for=condition=Established crd/installations.operator.tigera.io --timeout=300s

echo "=== creating the Calico Installation ==="
# custom-resources.yaml carries more than the Installation: v3.31 adds an
# APIServer, a Goldmane and a Whisker, and the operator registers those CRDs a
# moment after installations.operator.tigera.io. A single apply then fails on
# whichever kind arrived last ("no matches for kind Goldmane"). Retrying the
# apply covers every kind the stream ships, including ones it adds next.
for attempt in $(seq 24); do
  if kubectl apply -f /tmp/custom-resources.yaml; then
    break
  fi
  if [ "$attempt" -eq 24 ]; then
    echo "ERROR: custom-resources.yaml did not apply after 24 attempts" >&2
    exit 1
  fi
  echo "apply attempt $attempt failed; waiting for the operator's CRDs"
  sleep 5
done

echo "=== calico install submitted ==="
kubectl get pods -n tigera-operator -o wide 2>&1 | head -10
`

	out, exitCode, err := RunScriptWithTimeout(ctx, addr, "install-calico", script, timeout)
	if err != nil {
		return out, fmt.Errorf("cluster.InstallCalico: %w", err)
	}
	if exitCode != 0 {
		return out, fmt.Errorf("cluster.InstallCalico: script exited %d", exitCode)
	}
	return out, nil
}

// shellQuote renders s as a single-quoted shell word. The values interpolated
// here are a version string and a CIDR from this test's own configuration
// rather than anything untrusted, but they are going into a shell and quoting
// them costs nothing.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
