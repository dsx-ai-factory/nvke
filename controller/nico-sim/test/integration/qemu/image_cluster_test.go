//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package qemu

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	forgesdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/test/integration/testutil"
)

// TestNVKEImageCluster boots the actual reference image in two native VMs and
// uses its staged Debian packages to initialize and join a Kubernetes cluster.
// It is separate from the generic cloud-image lifecycle tests.
func TestNVKEImageCluster(t *testing.T) {
	if os.Getenv("NICO_VMS_TEST_NVKE_IMAGE") != "true" {
		t.Skip("set NICO_VMS_TEST_NVKE_IMAGE=true with NICO_VMS_BASE_IMAGE")
	}
	require.NotEmpty(t, os.Getenv("NICO_VMS_BASE_IMAGE"), "an explicit NVKE image is required")
	profile, err := testutil.BuildRealVMProfile(testutil.VMBackendDarwinQEMU)
	require.NoError(t, err)
	seed := testutil.DefaultSeedDoc()
	seed.InstanceTypes[0].CPUCount = 4
	seed.InstanceTypes[0].MemoryGiB = 6
	seed.InstanceTypes[0].DiskGiB = 30
	env := testutil.NewTestEnv(t, seed, profile)
	create := func(role string) string {
		name := testutil.UniqueInstanceName("nke-" + role)
		inst := env.CreateInstance(&forgesdk.InstanceCreateRequest{
			Name: name, TenantId: "d282929f-49d5-4dec-8d27-21ceb30ade19", InstanceTypeId: *forgesdk.NewNullableString(forgesdk.PtrString("itype-mock-001")), VpcId: "vpc-mock-001",
			Interfaces: []forgesdk.InterfaceCreateRequest{{SubnetId: forgesdk.PtrString("subnet-mock-001")}},
			UserData:   *forgesdk.NewNullableString(forgesdk.PtrString(testutil.RebootFlowUserData(name))),
		})
		t.Cleanup(func() {
			testutil.PreserveInstanceArtifacts(t, profile, inst.GetId())
			testutil.CleanupInstanceByID(t, env, profile, inst.GetId())
			if !testutil.KeepVMs() {
				testutil.WaitForInstanceDeletion(t, env, profile, inst.GetId())
			}
		})
		ready := testutil.WaitForInstanceState(t, env, profile, inst.GetId(), "ready")
		ip := testutil.InstanceIP(t, ready)
		testutil.WaitForSSHCommand(t, ip, testutil.TestSSHConfig(), "true", 3*time.Minute)
		return ip
	}
	cp, worker := create("cp"), create("worker")
	run := func(ip, label, command string) string {
		t.Helper()
		out, err := testutil.SSHRun(ip, testutil.TestSSHConfig(), command)
		// Join operations can contain credentials; only log output from other steps.
		if err != nil && label != "create join token" && label != "kubeadm join" && label != "kubeadm init" {
			t.Logf("%s output: %s", label, out)
		}
		require.NoError(t, err, "%s on %s failed", label, ip)
		return out
	}
	const install = `sudo bash -eu -c '
apt-get -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/nke-local.list -o Dir::Etc::sourceparts=- update
DEBIAN_FRONTEND=noninteractive apt-get -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/nke-local.list -o Dir::Etc::sourceparts=- install -y nvke-kubeadm-distro-1.34.2 nvke-cni-calico-manifests-3.30.1 nvke-cni-calico-images-3.30.1
swapoff -a
sysctl -w net.ipv4.ip_forward=1
systemctl is-active containerd
'`
	for _, ip := range []string{cp, worker} {
		run(ip, "install staged packages", install)
	}
	t.Log("Initializing control plane from staged Kubernetes v1.34.2")
	run(cp, "kubeadm init", fmt.Sprintf("sudo timeout 300 kubeadm init --kubernetes-version=v1.34.2 --apiserver-advertise-address=%s --control-plane-endpoint=%s:6443 --pod-network-cidr=10.244.0.0/16", cp, cp))
	join := strings.TrimSpace(run(cp, "create join token", "sudo kubeadm token create --print-join-command --ttl 10m"))
	require.True(t, strings.HasPrefix(join, "kubeadm join "), "unexpected kubeadm join response")
	run(worker, "kubeadm join", "sudo timeout 300 "+join)
	t.Log("Installing staged Calico with a pod CIDR outside the vmnet subnet")
	run(cp, "Calico install", `sudo bash -eu -c '
export KUBECONFIG=/etc/kubernetes/admin.conf
kubectl apply --server-side -f /usr/share/nvke-cni/calico/3.30.1/manifests/operator-crds.yaml
kubectl wait --for=condition=Established --timeout=120s -f /usr/share/nvke-cni/calico/3.30.1/manifests/operator-crds.yaml
kubectl apply --server-side -f /usr/share/nvke-cni/calico/3.30.1/manifests/tigera-operator.yaml
sed "s|192.168.0.0/16|10.244.0.0/16|g" /usr/share/nvke-cni/calico/3.30.1/manifests/custom-resources.yaml | kubectl apply -f -
kubectl wait nodes --all --for=condition=Ready --timeout=300s
kubectl -n kube-system rollout status deployment/coredns --timeout=180s
kubectl get nodes -o wide
'`)
	t.Log("Control plane and worker are Ready; CoreDNS is available")
	t.Log("Checking cross-node pod traffic and cluster DNS")
	run(cp, "pod network smoke", `sudo env KUBECONFIG=/etc/kubernetes/admin.conf bash -se <<'NVKE'
cp_node=$(kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}')
worker_node=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}')
kubectl apply -f - <<PODS
apiVersion: v1
kind: Pod
metadata:
  name: nvke-net-cp
spec:
  nodeName: $cp_node
  containers:
  - name: check
    image: docker.io/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
    command: ["sleep", "600"]
---
apiVersion: v1
kind: Pod
metadata:
  name: nvke-net-worker
spec:
  nodeName: $worker_node
  containers:
  - name: check
    image: docker.io/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
    command: ["sleep", "600"]
PODS
kubectl wait pod/nvke-net-cp pod/nvke-net-worker --for=condition=Ready --timeout=180s
cp_ip=$(kubectl get pod nvke-net-cp -o jsonpath='{.status.podIP}')
worker_ip=$(kubectl get pod nvke-net-worker -o jsonpath='{.status.podIP}')
kubectl exec nvke-net-worker -- ping -c 3 -W 3 "$cp_ip"
kubectl exec nvke-net-cp -- ping -c 3 -W 3 "$worker_ip"
kubectl exec nvke-net-worker -- nslookup kubernetes.default.svc.cluster.local
NVKE`)
	t.Log("Cross-node pod traffic and cluster DNS passed")
}
