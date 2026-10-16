#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Isolate the VMaaS offload runtime from the DPU cluster's primary OVN."""
import json

from lab import command, require

NAMESPACE = "vmaas-offload"
KUBE_NAMESPACE = "ovn-kubernetes-vmaas"
RUN_DIR = "/var/run/nke-vmaas-offload"


def exec_offload(lab, node, *args, **kwargs):
    return lab.exec(node, "ip", "netns", "exec", NAMESPACE, "env", "OVS_RUNDIR=" + RUN_DIR, *args, **kwargs)


def setup(lab):
    for index, node in enumerate(lab.dpus):
        if node.get("offload_ready"):
            continue
        node["offload_ip"] = f"172.28.237.{2 + 4 * index}"
        gateway = f"172.28.237.{1 + 4 * index}"
        lab.exec(node["container"], "ip", "netns", "add", NAMESPACE)
        lab.exec(node["container"], "ip", "link", "add", "vmaas-mgmt", "type", "veth", "peer", "name", "vmaas-peer")
        lab.exec(node["container"], "ip", "link", "set", "vmaas-peer", "netns", NAMESPACE)
        lab.exec(node["container"], "ip", "address", "add", gateway + "/30", "dev", "vmaas-mgmt")
        lab.exec(node["container"], "ip", "link", "set", "vmaas-mgmt", "up")
        for link in ["lo", "vmaas-peer"]:
            exec_offload(lab, node["container"], "ip", "link", "set", link, "up")
        exec_offload(lab, node["container"], "ip", "link", "set", "vmaas-peer", "name", "eth0")
        exec_offload(lab, node["container"], "ip", "address", "add", node["offload_ip"] + "/30", "dev", "eth0")
        exec_offload(lab, node["container"], "ip", "route", "add", "default", "via", gateway)
        lab.exec(node["container"], "sysctl", "-w", "net.ipv4.ip_forward=1")
        exec_offload(lab, node["container"], "sysctl", "-w", "net.ipv4.ip_forward=1")
        for scope in ["all", "default", "lo"]:
            exec_offload(lab, node["container"], "sysctl", "-w", "net.ipv6.conf." + scope + ".disable_ipv6=0")
        # API traffic leaves through the DPU's management interface. Direct
        # routes between offload namespaces preserve OVN's tunnel endpoint IPs.
        lab.exec(node["container"], "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", node["offload_ip"] + "/32",
                 "!", "-d", "172.28.237.0/24", "-j", "MASQUERADE")
        lab.exec(node["container"], "iptables", "-I", "FORWARD", "1", "-i", "vmaas-mgmt", "-j", "ACCEPT")
        lab.exec(node["container"], "iptables", "-I", "FORWARD", "1", "-o", "vmaas-mgmt", "-j", "ACCEPT")
        lab.exec(node["container"], "mkdir", "-p", RUN_DIR, "/etc/nke-vmaas-offload", "/var/lib/nke-vmaas-offload",
                 "/var/log/nke-vmaas-offload", "/var/run/nke-vmaas-cni")
        lab.exec(node["container"], "ovsdb-tool", "create", "/etc/nke-vmaas-offload/conf.db", "/usr/share/openvswitch/vswitch.ovsschema")
        exec_offload(lab, node["container"], "ovsdb-server", "/etc/nke-vmaas-offload/conf.db", "--detach",
                     "--remote=punix:" + RUN_DIR + "/db.sock", "--pidfile=" + RUN_DIR + "/ovsdb-server.pid",
                     "--unixctl=" + RUN_DIR + "/ovsdb-server.ctl", "--log-file=/var/log/nke-vmaas-offload/ovsdb-server.log")
        exec_offload(lab, node["container"], "ovs-vsctl", "--no-wait", "init")
        exec_offload(lab, node["container"], "ovs-vsctl", "--no-wait", "set", "Open_vSwitch", ".",
                     "external_ids:system-id=" + node["node_id"])
        exec_offload(lab, node["container"], "ovs-vswitchd", "unix:" + RUN_DIR + "/db.sock", "--detach",
                     "--pidfile=" + RUN_DIR + "/ovs-vswitchd.pid", "--unixctl=" + RUN_DIR + "/ovs-vswitchd.ctl",
                     "--log-file=/var/log/nke-vmaas-offload/ovs-vswitchd.log")
        # The OVN startup script derives control socket names from these PIDs.
        lab.exec(node["container"], "sh", "-ec", 'for daemon in ovsdb-server ovs-vswitchd; do '
                 'pid=$(cat "$1/$daemon.pid"); ln -sfn "$daemon.ctl" "$1/$daemon.$pid.ctl"; done', "ovs-controls", RUN_DIR)
        node["offload_ready"] = True
        lab.save()
    for node in lab.dpus:
        for peer in lab.dpus:
            if node is not peer:
                lab.exec(node["container"], "ip", "route", "replace", peer["offload_ip"] + "/32", "via", peer["ip"])


def isolate_pod(spec, address=None):
    """Run all offload daemons and probes in the dedicated network namespace."""
    mappings = {"/var/run/openvswitch": RUN_DIR, "/var/run/ovn": RUN_DIR,
                "/var/lib/openvswitch": "/var/lib/nke-vmaas-offload", "/etc/openvswitch": "/etc/nke-vmaas-offload",
                "/var/log/openvswitch": "/var/log/nke-vmaas-offload", "/var/log/ovn": "/var/log/nke-vmaas-offload",
                "/var/log/ovn-kubernetes": "/var/log/nke-vmaas-offload", "/var/run/ovn-kubernetes": "/var/run/nke-vmaas-cni"}
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
                require(address is not None, "An offload HTTP probe needs its namespace address")
                probe["httpGet"]["host"] = address
