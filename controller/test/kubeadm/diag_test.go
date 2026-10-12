//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/test/kubeadm/internal/cluster"
	"github.com/dsx-ai-factory/nvke/controller/test/kubeadm/internal/nicovms"
)

// On-VM post-mortems, run through the node-agent's ExecuteScript RPC rather
// than SSH so they work even when only gRPC is reachable. Both are best-effort:
// they are called from a path that is already failing, so they log and return
// rather than failing the test a second time.
//
// The agent writes script_contents to a file and exec()s it directly, so the
// shebang is required — without it the exec fails with ENOEXEC.

// joinFailureDiag captures why a join failed on the joining node: the nke-agent
// journal (which carries the actual kubeadm output), the node's own networking,
// and reachability to the control plane's API and etcd ports. That last part is
// what distinguishes a kubeadm-side error from a bridge connectivity problem,
// which is the failure mode most likely to be specific to a CI runner.
func joinFailureDiag(ctx context.Context, t *testing.T, nodeName, nodeIP, initIP string) {
	t.Helper()
	script := "#!/bin/bash\n" +
		"set +e\n" +
		"INIT=" + initIP + "\n" +
		`echo "=== nke-agent journal (last 200 lines; carries the kubeadm output) ==="
journalctl -u nke-agent --no-pager -n 200 2>&1 | tail -n 200
echo "=== node networking ==="
ip -o addr show; echo "-- routes --"; ip route; echo "-- resolv --"; cat /etc/resolv.conf 2>&1
echo "=== inter-VM reachability to the control plane $INIT ==="
for p in 6443 2379 2380; do
  timeout 5 bash -c "echo > /dev/tcp/$INIT/$p" 2>/dev/null && echo "tcp $INIT:$p OPEN" || echo "tcp $INIT:$p UNREACHABLE"
done
echo "-- curl the apiserver --"
curl -k -sS --max-time 5 "https://$INIT:6443/healthz" 2>&1 | head -3; echo "(curl rc=$?)"
echo "-- ping --"; ping -c2 -W2 "$INIT" 2>&1 | tail -3
echo "=== kubelet journal (last 120) ==="
journalctl -u kubelet --no-pager -n 120 2>&1 | tail -120`

	addr := agentAddr(nodeIP, cluster.DefaultAgentPort)
	out, err := cluster.RunScript(ctx, addr, "join-failure-diag", script)
	if err != nil {
		t.Logf("post-mortem: ExecuteScript on %s failed: %v", nodeName, err)
		return
	}
	t.Logf("=== POST-MORTEM on %s (join failure) ===\n%s", nodeName, out)
}

// initNodeDiag captures the control-plane side: reverse reachability to the
// other node (the direction the node-side check cannot cover, and an asymmetric
// bridge is a real possibility), plus etcd and kube-system state.
//
// Unlike the 4-VM original this cluster has a single control plane, so the etcd
// member list is expected to show exactly one member and there is no learner
// promotion to inspect. The output is still worth having: an unhealthy
// single-member etcd is a perfectly good explanation for a failed init.
func initNodeDiag(ctx context.Context, t *testing.T, initName, initIP, otherIP string) {
	t.Helper()
	script := "#!/bin/bash\n" +
		"set +e\n" +
		// The agent runs scripts with its own PATH, which need not include the
		// directory the Kubernetes tools landed in.
		"export PATH=$PATH:/usr/local/bin:/usr/local/sbin\n" +
		"OTHER=" + otherIP + "\n" +
		`echo "=== provisioning: are the tools even here? ==="
for b in kubeadm kubelet kubectl containerd runc crictl; do
  printf '%-11s %s\n' "$b" "$(command -v "$b" || echo MISSING)"
done
echo "=== cloud-init output (last 120) ==="
tail -n 120 /var/log/cloud-init-output.log 2>&1
echo "=== reverse reachability: control plane -> $OTHER ==="
for p in 2379 2380 6443 10250; do
  timeout 5 bash -c "echo > /dev/tcp/$OTHER/$p" 2>/dev/null && echo "tcp $OTHER:$p OPEN" || echo "tcp $OTHER:$p UNREACHABLE"
done
ping -c2 -W2 "$OTHER" 2>&1 | tail -2
echo "=== nke-agent journal (last 200) ==="
journalctl -u nke-agent --no-pager -n 200 2>&1 | tail -n 200
echo "=== etcd member list / health (single member expected) ==="
CID=$(crictl ps --name etcd -q 2>/dev/null | head -1)
echo "etcd container: ${CID:-<none>}"
E="etcdctl --endpoints=https://127.0.0.1:2379 --cacert=/etc/kubernetes/pki/etcd/ca.crt --cert=/etc/kubernetes/pki/etcd/server.crt --key=/etc/kubernetes/pki/etcd/server.key"
if [ -n "$CID" ]; then
  echo "-- member list --"; crictl exec "$CID" $E member list -w table 2>&1
  echo "-- endpoint health --"; crictl exec "$CID" $E endpoint health 2>&1
fi
echo "=== etcd static-pod log (last 80) ==="
LID=$(crictl ps -a --name etcd -q 2>/dev/null | head -1)
[ -n "$LID" ] && crictl logs --tail 80 "$LID" 2>&1 | tail -80 || echo "(no etcd container via crictl)"
echo "=== kube-system pods (CNI is NOT installed: NotReady/Pending is expected) ==="
KUBECONFIG=/etc/kubernetes/admin.conf kubectl get pods -n kube-system -o wide 2>&1 | head -25
echo "=== nodes ==="
KUBECONFIG=/etc/kubernetes/admin.conf kubectl get nodes -o wide 2>&1 | head -10`

	addr := agentAddr(initIP, cluster.DefaultAgentPort)
	out, err := cluster.RunScript(ctx, addr, "init-node-diag", script)
	if err != nil {
		t.Logf("post-mortem: ExecuteScript on control plane %s failed: %v", initName, err)
		return
	}
	t.Logf("=== POST-MORTEM on control plane %s (etcd + reverse-net) ===\n%s", initName, out)
}

// saveConsoleLogs copies each VM's serial-console log out of its instance
// directory, which deleting the instance removes.
//
// nico-vms builds the overlay as <workDir>/instances/<id>/disk.qcow2, and
// libvirt writes <overlay>.ttyS0.log and .ttyS1.log beside it. The name carries
// the instance ID so teardown-ci-host.sh can flatten them into one directory
// without collisions.
func saveConsoleLogs(t *testing.T, workDir string, nodes []nicovms.Node) {
	t.Helper()
	saved := 0
	for _, n := range nodes {
		dir := filepath.Join(workDir, "instances", n.InstanceID)
		matches, err := filepath.Glob(filepath.Join(dir, "*.ttyS*.log"))
		if err != nil || len(matches) == 0 {
			continue
		}
		for _, src := range matches {
			body, rerr := os.ReadFile(src)
			if rerr != nil {
				t.Logf("read console log %s: %v", src, rerr)
				continue
			}
			dest := filepath.Join(workDir, n.InstanceID+"-"+n.Name+"-"+filepath.Base(src))
			if werr := os.WriteFile(dest, body, 0o644); werr != nil {
				t.Logf("save console log %s: %v", dest, werr)
				continue
			}
			saved++
		}
	}
	t.Logf("saved %d console log(s) into %s before deleting the instances", saved, workDir)
}
