#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Set up an isolated network environment on each DPU for workload cluster networking.

Create the namespace and connect it to the DPU's main network for API access.
Start a separate OVS instance. Other modules later connect the workload hosts and fabric.
"""
import json

from .runtime import Simulation, command, require

NAMESPACE = "workload-offload"
KUBE_NAMESPACE = "ovn-kubernetes-workload"
RUN_DIR = "/var/run/nvke-nais-dpu-sim-offload"


# Goal: give every DPU a second, isolated network for the workload-offload OVS/OVN runtime, so it can
# run next to the DPU cluster's primary OVN without the two colliding.
#
# Per DPU (the loop runs once for each entry in sim.dpus):
#   root namespace     the DPU's normal network, where the primary OVN runs. By the time setup() runs,
#                      primary OVN has already moved the management IP from eth0 onto its bridge, breth0
#   offload namespace  a private network holding only the offload runtime and its own OVS daemons
#   veth cable         joins the two; its root-side end is the "gateway". This cable is the offload
#                      runtime's path to the Kubernetes APIs, not the pod data path
#
# devices.py later adds a fabric NIC (eth1/breth1) to the offload namespace.
# In this no-overlay setup, BGP supplies routes for pod traffic across the fabric without encapsulation.
# A live test between pods on different hosts observed traffic on eth1, with none on eth0 or in GENEVE packets.
#
# The offload IP (.2) is unique per DPU because OVN requires every node to register an "encap" address.
# devices.py sets ovn-encap-ip to this address. OVS creates GENEVE tunnel ports even in this no-overlay setup.
# The observed pod traffic used the fabric path instead of those tunnels.
# The simulation assigns addresses from its fixed private range, 172.28.237.0/24; Linux does not choose them.
# Each DPU gets a separate /30 subnet, a block of four addresses with two usable interface addresses.
# For DPU index i, setup() assigns .(1 + 4*i) to workload-mgmt and .(2 + 4*i) to its peer.
# The frames below follow the first DPU. The others use .5/.6, .9/.10, and so on.
#
# Read the frames top to bottom like a flip-book. '═' is the cable, '◄ NEW' is what the command added.
#
# [0] START (before the loop body)
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ (does not exist yet)                 │
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [1] Create the workload-offload network namespace.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  DOWN                             │  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [2] Create the connected workload-mgmt and workload-peer interfaces in the DPU root namespace.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  DOWN                             │
# │ workload-mgmt   DOWN, no IP  ═╗ veth             │     │                                      │  ◄ NEW
# │ workload-peer   DOWN, no IP  ═╝ pair             │     │                                      │  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [3] Move workload-peer into the workload-offload namespace.
# Keep workload-mgmt in the DPU root namespace.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  DOWN                             │
# │ workload-mgmt   DOWN, no IP                      │═════│ workload-peer   DOWN, no IP          │  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [4] Assign 172.28.237.1/30 to workload-mgmt in the DPU root namespace.
# Enable the workload-mgmt interface.
# Here /30 is the prefix length: 30 network bits, equivalent to subnet mask 255.255.255.252.
# The first subnet spans 172.28.237.0 through 172.28.237.3: .0 identifies the network and .3 is broadcast.
# Its usable addresses are .1 for workload-mgmt (the gateway) and .2 for the peer, assigned in [7].
# Linux automatically adds a route for 172.28.237.0/30 through workload-mgmt when the address is assigned and the interface is enabled.
# This route sends traffic directly to that subnet without a gateway.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  DOWN                             │
# │ workload-mgmt   172.28.237.1/30  UP              │═════│ workload-peer   DOWN, no IP          │  ◄ NEW
# │ route 172.28.237.0/30 dev workload-mgmt (auto)   │     │                                      │  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [5] Enable the lo and workload-peer interfaces inside the workload-offload namespace.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  UP                               │  ◄ NEW
# │ workload-mgmt   172.28.237.1/30  UP              │═════│ workload-peer   UP, no IP            │  ◄ NEW
# │ route 172.28.237.0/30 dev workload-mgmt (auto)   │     │                                      │
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [6] Rename workload-peer to eth0 inside the workload-offload namespace.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  UP                               │
# │ workload-mgmt   172.28.237.1/30  UP              │═════│ eth0   UP, no IP                     │  ◄ NEW
# │ route 172.28.237.0/30 dev workload-mgmt (auto)   │     │                                      │
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [7] Assign 172.28.237.2/30 to eth0 inside the workload-offload namespace.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  UP                               │
# │ workload-mgmt   172.28.237.1/30  UP              │═════│ eth0   172.28.237.2/30  UP           │  ◄ NEW
# │ route 172.28.237.0/30 dev workload-mgmt (auto)   │     │ route 172.28.237.0/30 dev eth0 (auto)│  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [8] Set 172.28.237.1 as the default gateway inside the workload-offload namespace.
# Traffic without a more specific route uses this gateway.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  UP                               │
# │ workload-mgmt   172.28.237.1/30  UP              │═════│ eth0   172.28.237.2/30  UP           │
# │ route 172.28.237.0/30 dev workload-mgmt (auto)   │     │ route 172.28.237.0/30 dev eth0 (auto)│
# │                                                  │     │ route default via 172.28.237.1       │  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [9] Allow API requests from workload-offload to cross the veth pair and pass through the DPU root namespace.
# Enable IPv4 forwarding in both namespaces and allow forwarded traffic through workload-mgmt in both directions.
# For destinations outside 172.28.237.0/24, MASQUERADE replaces the offload source IP with the DPU's outgoing interface IP.
# Linux tracks that replacement and sends API replies back to the original offload IP through the veth pair.
#
# Between DPUs, a request from offload IP .2 to offload IP .6 keeps source .2 because the destination is exempt.
# Step [11] tells each DPU root namespace to reach another DPU's offload IP through that DPU's management IP.
#
# Enable IPv6 inside workload-offload as a separate setting. This does not assign addresses or change IPv4 routes.
# Routes choose the outgoing interface. devices.py later adds eth1/breth1 for pod traffic, so that interface is absent from this diagram.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  UP                               │
# │ workload-mgmt   172.28.237.1/30  UP              │═════│ eth0   172.28.237.2/30  UP           │
# │                                                  │     │ route default via 172.28.237.1       │  ◄ NEW
# | IPv4 forwarding enabled                          |     | IPv4 forwarding enabled              │  ◄ NEW
# │ FORWARD: accept in/out workload-mgmt             │     │ IPv6 enabled (all/default/lo)        │  ◄ NEW
# │ NAT: src .2, dst not in 172.28.237.0/24          │     │                                      │  ◄ NEW
# │      -> MASQUERADE (leaves as <dpu-mgmt-ip>)     │     │                                      │  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [10] Create a separate OVS database for the workload runtime.
# Start its database server and packet-switching process inside the workload-offload namespace.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ lo  UP                               │
# │ workload-mgmt   172.28.237.1/30  UP              │═════│ eth0   172.28.237.2/30  UP           │
# │ (DPU's primary OVS/OVN is untouched)             │     │ ovsdb-server  (own conf.db, db.sock) │  ◄ NEW
# │                                                  │     │ ovs-vswitchd   (own OVS instance)    │  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
#
# [11] After all DPUs have offload addresses, add peer routes in each DPU root namespace.
# Route each peer offload address through that peer DPU's management address.
# ┌─ DPU root netns ─────────────────────────────────┐     ┌─ ns: workload-offload ───────────────┐
# │ breth0 <dpu-mgmt-ip> ──► other DPUs / API        │     │ eth0   172.28.237.2/30  UP           │
# │ workload-mgmt   172.28.237.1/30  UP              │═════│ OVS runs here                        │
# │ route 172.28.237.6/32 via <peer-dpu-ip>          │     │                                      │  ◄ NEW
# │   (no MASQUERADE: OVN encap IPs kept as-is)      │     │                                      │  ◄ NEW
# └──────────────────────────────────────────────────┘     └──────────────────────────────────────┘
def setup(sim: Simulation):
    for index, node in enumerate(sim.dpus):
        if node.get("offload_ready"):
            continue
        # Each DPU gets its own /30 carved from 172.28.237.0/24: .1 is the gateway (root-ns side),
        # .2 is the offload namespace. Index 0 -> .1/.2, index 1 -> .5/.6, index 2 -> .9/.10, ...
        node["offload_ip"] = f"172.28.237.{2 + 4 * index}"
        gateway = f"172.28.237.{1 + 4 * index}"
        sim.exec(node["container"], "ip", "netns", "add", NAMESPACE)
        sim.exec(node["container"], "ip", "link", "add", "workload-mgmt", "type", "veth", "peer", "name", "workload-peer")
        sim.exec(node["container"], "ip", "link", "set", "workload-peer", "netns", NAMESPACE)
        sim.exec(node["container"], "ip", "address", "add", gateway + "/30", "dev", "workload-mgmt")
        sim.exec(node["container"], "ip", "link", "set", "workload-mgmt", "up")
        for link in ["lo", "workload-peer"]:
            exec_in_dpu_workload_network(sim, node["container"], "ip", "link", "set", link, "up")
        exec_in_dpu_workload_network(sim, node["container"], "ip", "link", "set", "workload-peer", "name", "eth0")
        exec_in_dpu_workload_network(sim, node["container"], "ip", "address", "add", node["offload_ip"] + "/30", "dev", "eth0")
        exec_in_dpu_workload_network(sim, node["container"], "ip", "route", "add", "default", "via", gateway)
        sim.exec(node["container"], "sysctl", "-w", "net.ipv4.ip_forward=1")
        exec_in_dpu_workload_network(sim, node["container"], "sysctl", "-w", "net.ipv4.ip_forward=1")
        # Containers can start with IPv6 disabled.
        for scope in ["all", "default", "lo"]:
            exec_in_dpu_workload_network(sim, node["container"], "sysctl", "-w", "net.ipv6.conf." + scope + ".disable_ipv6=0")
        # Keep peer encap traffic unmodified. Translate other traffic from the offload IP
        # to the outgoing interface address so API replies can return.
        sim.exec(node["container"], "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", node["offload_ip"] + "/32",
                 "!", "-d", "172.28.237.0/24", "-j", "MASQUERADE")
        # Insert both rules before existing DROP or REJECT rules.
        sim.exec(node["container"], "iptables", "-I", "FORWARD", "1", "-i", "workload-mgmt", "-j", "ACCEPT")
        sim.exec(node["container"], "iptables", "-I", "FORWARD", "1", "-o", "workload-mgmt", "-j", "ACCEPT")
        # Separate OVS directories prevent collisions with the primary runtime.
        # isolate_pod() maps the workload OVN mounts to these paths.
        sim.exec(node["container"], "mkdir", "-p", RUN_DIR, "/etc/nvke-nais-dpu-sim-offload", "/var/lib/nvke-nais-dpu-sim-offload",
                 "/var/log/nvke-nais-dpu-sim-offload", "/var/run/nvke-nais-dpu-sim-cni")
        sim.exec(node["container"], "ovsdb-tool", "create", "/etc/nvke-nais-dpu-sim-offload/conf.db", "/usr/share/openvswitch/vswitch.ovsschema")
        exec_in_dpu_workload_network(sim, node["container"], "ovsdb-server", "/etc/nvke-nais-dpu-sim-offload/conf.db", "--detach",
                     "--remote=punix:" + RUN_DIR + "/db.sock", "--pidfile=" + RUN_DIR + "/ovsdb-server.pid",
                     "--unixctl=" + RUN_DIR + "/ovsdb-server.ctl", "--log-file=/var/log/nvke-nais-dpu-sim-offload/ovsdb-server.log")
        exec_in_dpu_workload_network(sim, node["container"], "ovs-vsctl", "--no-wait", "init")
        exec_in_dpu_workload_network(sim, node["container"], "ovs-vsctl", "--no-wait", "set", "Open_vSwitch", ".",
                     "external_ids:system-id=" + node["node_id"])
        exec_in_dpu_workload_network(sim, node["container"], "ovs-vswitchd", "unix:" + RUN_DIR + "/db.sock", "--detach",
                     "--pidfile=" + RUN_DIR + "/ovs-vswitchd.pid", "--unixctl=" + RUN_DIR + "/ovs-vswitchd.ctl",
                     "--log-file=/var/log/nvke-nais-dpu-sim-offload/ovs-vswitchd.log")
        # The OVN startup script derives control socket names from these PIDs.
        sim.exec(node["container"], "sh", "-ec", 'for daemon in ovsdb-server ovs-vswitchd; do '
                 'pid=$(cat "$1/$daemon.pid"); ln -sfn "$daemon.ctl" "$1/$daemon.$pid.ctl"; done', "ovs-controls", RUN_DIR)
        node["offload_ready"] = True
        sim.save()
    # Wait until every DPU has an offload IP before adding peer routes.
    # These routes preserve reachability between encap addresses. The observed pod traffic bypasses them.
    # That observation does not establish whether removing the routes is safe during startup or restarts.
    for node in sim.dpus:
        for peer in sim.dpus:
            if node is not peer:
                sim.exec(node["container"], "ip", "route", "replace", peer["offload_ip"] + "/32", "via", peer["ip"])


def isolate_pod(spec, address=None):
    """Run all offload daemons and probes in the dedicated network namespace."""
    mappings = {"/var/run/openvswitch": RUN_DIR, "/var/run/ovn": RUN_DIR,
                "/var/lib/openvswitch": "/var/lib/nvke-nais-dpu-sim-offload", "/etc/openvswitch": "/etc/nvke-nais-dpu-sim-offload",
                "/etc/ovn": "/etc/nvke-nais-dpu-sim-offload",
                "/etc/cni/net.d": "/etc/nvke-nais-dpu-sim-offload/cni", "/opt/cni/bin": "/opt/nvke-nais-dpu-sim-offload/cni",
                "/var/log/openvswitch": "/var/log/nvke-nais-dpu-sim-offload", "/var/log/ovn": "/var/log/nvke-nais-dpu-sim-offload",
                "/var/log/ovn-kubernetes": "/var/log/nvke-nais-dpu-sim-offload", "/var/run/ovn-kubernetes": "/var/run/nvke-nais-dpu-sim-cni"}
    for volume in spec.get("volumes", []):
        if path := volume.get("hostPath", {}).get("path"):
            if path.rstrip("/") in mappings:
                volume["hostPath"]["path"] = mappings[path.rstrip("/")]
                volume["hostPath"]["type"] = "DirectoryOrCreate"
    volumes = spec.setdefault("volumes", [])
    netns = next((v for v in volumes if v.get("hostPath", {}).get("path", "").rstrip("/") == "/var/run/netns"), None)
    if netns is None:
        netns = {"name": "offload-netns", "hostPath": {"path": "/var/run/netns", "type": "Directory"}}
        volumes.append(netns)
    prefix = ["ip", "netns", "exec", NAMESPACE]
    for container in spec.get("initContainers", []) + spec["containers"]:
        image_config = json.loads(command("docker", "image", "inspect", container["image"]))[0]["Config"]
        entrypoint = container.get("command", image_config.get("Entrypoint") or [])
        arguments = container.pop("args", [] if "command" in container else image_config.get("Cmd") or [])
        container["command"] = prefix + entrypoint + arguments
        container.setdefault("securityContext", {}).update(privileged=True, runAsUser=0, allowPrivilegeEscalation=True)
        mounts = container.setdefault("volumeMounts", [])
        mount = next((m for m in mounts if m["mountPath"].rstrip("/") == "/var/run/netns"), None)
        if mount is None:
            mount = {"name": netns["name"], "mountPath": "/var/run/netns"}
            mounts.append(mount)
        mount.update(readOnly=True, mountPropagation="HostToContainer")
        for key in ["readinessProbe", "livenessProbe", "startupProbe"]:
            probe = container.get(key, {})
            if "exec" in probe:
                probe["exec"]["command"] = prefix + probe["exec"]["command"]
            if "httpGet" in probe:
                if address is not None:
                    probe["httpGet"]["host"] = address
                else:
                    http = probe.pop("httpGet")
                    require(http.get("scheme", "HTTP") == "HTTP", "Expected an HTTP metrics probe")
                    probe["exec"] = {"command": prefix + ["curl", "--fail", "--silent", "--show-error", "--output", "/dev/null",
                        f"http://127.0.0.1:{http['port']}{http.get('path', '/')}" ]}


def exec_in_dpu_workload_network(sim, node, *args, **kwargs):
    return sim.exec(node, "ip", "netns", "exec", NAMESPACE, "env", "OVS_RUNDIR=" + RUN_DIR, *args, **kwargs)
