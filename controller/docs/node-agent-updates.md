# In-place control-plane node-agent updates

NVKE can update the node agent without replacing a bare-metal node. This feature
covers control planes managed by the control-plane controller. Workers and
pool-managed control planes are outside its scope.

Set `nodeAgent.updateMethod` to `apt` or `bundle` in `nke-controller-config`.
In Helm, set `nodeAgent.updateMethod`. The installation method comes from the controller
configuration, not the cluster request.

Set `controlPlaneConfig.nodeAgentConfig.version` to an exact release version in
`CreateCluster` or `UpdateCluster`. For example, a cluster update can include:

```json
{
  "controlPlaneConfig": {
    "nodeAgentConfig": {
      "version": "v0.1.33"
    }
  }
}
```

NVKE also accepts `0.1.33` and stores `v0.1.33`.
It accepts exact prerelease versions, such as `v0.3.0-rc.2`.
It rejects partial versions, build suffixes, and `latest`. It permits downgrades.

Agents that reject prerelease targets need an initial update outside this path.
Both the controller and the running agent must accept the target version.

If an update omits the version, NVKE keeps the stored target. An explicit empty
version disables future convergence. An accepted installation finishes before
NVKE uses a changed target or stops scheduling updates.

New nodes start with the agent baked into the OS image. NVKE installs the desired
version before preflight and kubeadm. Nodes that already run that version
continue without another installation.

If you opt in, keep the OS image's agent version equal to the cluster's desired
version during rolling node updates. Before a rolling update, update the image
or the desired version so that both match. A newer image does not change the
cluster's desired version automatically.

The control-plane controller updates joined nodes one at a time. It retries the
same node after failures. `GetCluster.nodeAgentUpgrade` reports the accepted
target, current phase, and latest error. Changing the desired version does not
interrupt an accepted installation.

Installation runs in an independent systemd unit. Agent restarts do not stop
that unit. Older agents use the controller's bootstrap runbook. That path needs
Python 3, systemd, and the artifact tools included in the reference node image.
Upgrade and downgrade use `StartNodeAgentUpgrade` with the requested version.
Use `GetNodeAgentUpgradeStatus` to read the installation status.
APT installs use the node's configured repositories. Bundle installs use its
configured agent bundle source.

Node deletion or remediation stops further upgrade attempts for that node.
An accepted installation does not block the existing deletion workflow.
Cluster deletion also continues if the agent cannot respond.

See the [selected design](../../agent/docs/in-place-update/controller-driven-desired-state-design.md)
for the lifecycle and compatibility contract.
