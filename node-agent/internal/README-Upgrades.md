# Upgrade Process using the NKE Node-Agent

Orchestration of the upgrade process on a node is expected to be handled by the caller, the Node Agent provides 
grouped operations to aid in the process.

## Process
1. Upgrade the Master Control Plane node first
2. Upgrade additional Control Plane nodes
3. Upgrade worker nodes

## ControlPlane Nodes

Be sure to follow the process and update all Control Plane nodes before attempting to update any
worker nodes.  It's important to verify that each node succesfully completes the upgrade steps before
moving on to the next node in the cluster.

### ControlPlane Master Node

1. UpdateKubeadmPackage({NewVersion: "v1.29.9"}) 
2. DrainNode({NodeName: <name-of-node-being-upgraded>}) *Always executed on Master Node
3. UpdateKubeletPackage({NewVersion:" v1.29.9"})
4. UncordonNode({NodeName: <name-of-node-being-upgraded>}) *Always executed on Master Node

Repeat steps 1-4 for each additional ControlPlane Node in the cluster, note that the DrainNode and UncordonNode
calls MUST be run against the ControlPlane Master Node!

## Worker Nodes

Worker Nodes should NOT be upgraded until the ControlPlane Nodes have been succesfully upgraded!
It's advised to upgrade Worker Nodes one at a time to ensure you don't run out of capacity for workloads
running in your cluster.

The steps to upgrade a Worker Node are the same steps we use for a ControlPlane Node:
1. UpdateKubeadmPackage({NewVersion: "v1.29.9"}) 
2. DrainNode({NodeName: <name-of-node-being-upgraded>}) *Always executed on Master Node
3. UpdateKubeletPackage({NewVersion:" v1.29.9"})
4. UncordonNode({NodeName: <name-of-node-being-upgraded>}) *Always executed on Master Node

## Monitoring Upgrade Progress

The upgrade process for Kubeadm and Kubelet are backgrounded processes.  You can monitor the progress of these operations 
a couple of ways:
1. Watch the "UpgradePhase" and "UpgradeStatus" fields in the NodeStatus object
2. Watch the "GetNodes" response and wait for node to become Ready with correct version




