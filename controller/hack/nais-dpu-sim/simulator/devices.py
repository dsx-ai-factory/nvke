# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Connect simulated host devices, representors, and uplinks."""
import hashlib
import json


from .runtime import command
from .dpu_workload_network import NAMESPACE as OFFLOAD_NAMESPACE


def wire(sim):
    driver = sim.config["driver"]
    for pair, host in enumerate(sim.hosts):
        if host.get("wired"):
            continue
        host_pid = json.loads(command("docker", "inspect", host["container"]))[0]["State"]["Pid"]
        dpu_pid = json.loads(command("docker", "inspect", host["dpu"]))[0]["State"]["Pid"]
        sim.exec(driver, "nsenter", "-t", str(host_pid), "-n", "sh", "-ec",
                  'for i in $(seq 0 15); do if ! ip link show "eth0-$i" >/dev/null 2>&1; then '
                  'ip link add "eth0-$i" type veth peer name "rep0-$i"; ip link set "rep0-$i" netns "$1"; fi; '
                  'ip link set "eth0-$i" up; done', "wire", str(dpu_pid))
        sim.exec(host["dpu"], "sh", "-ec", 'for i in $(seq 0 15); do if ip link show "rep0-$i" >/dev/null 2>&1; then '
                  'ip link set "rep0-$i" netns "$1"; fi; done', "offload", OFFLOAD_NAMESPACE)
        sim.dpu_exec(host["dpu"], "sh", "-ec", 'for i in $(seq 0 15); do ip link set "rep0-$i" up; done')
        gateway = f"172.28.234.{20 + pair}"
        sim.exec(host["container"], "ip", "addr", "replace", gateway + "/24", "dev", "eth0-0")
        network = sim.name + "-gateway"
        details = json.loads(command("docker", "inspect", host["dpu"]))[0]
        if network not in details["NetworkSettings"]["Networks"]:
            command("docker", "network", "connect", network, host["dpu"])
        address = json.loads(command("docker", "inspect", host["dpu"]))[0]["NetworkSettings"]["Networks"][network]["IPAddress"]
        sim.exec(host["dpu"], "sh", "-ec", 'if ip link show eth1 >/dev/null 2>&1; then ip link set eth1 netns "$1"; fi',
                  "offload", OFFLOAD_NAMESPACE)
        sim.dpu_exec(host["dpu"], "sh", "-ec",
                  'ovs-vsctl --may-exist add-br breth1 -- set Bridge breth1 datapath_type=system; '
                  'ovs-vsctl --may-exist add-port breth1 eth1 -- --may-exist add-port breth1 rep0-0; '
                  'ip addr flush dev eth1; ip link set eth1 up; ip link set breth1 up; '
                  'ip addr replace "$1/24" dev breth1', "gateway", address)
        dpu = next(n for n in sim.dpus if n["container"] == host["dpu"])
        sim.dpu_exec(host["dpu"], "ovs-vsctl", "set", "Open_vSwitch", ".",
                  "external_ids:host-k8s-nodename=" + host["name"],
                  "external_ids:hostname=" + sim.exec(host["dpu"], "hostname"),
                  "external_ids:ovn-encap-ip=" + dpu["offload_ip"],
                  "external_ids:ovn-encap-type=geneve", "external_ids:ovn-gw-interface=eth1",
                  "external_ids:ovn-gw-router-subnet=172.28.234.0/24", "external_ids:ovn-gw-nexthop=172.28.234.254",
                  "external_ids:ovn-bridge-datapath-type=system")
        print(f"Paired {host['name']} <-> {host['dpu']}, 16 VF links", flush=True)
        host["wired"] = True
        sim.save()


def simulation_links(sim):
    network = sim.name + "-uplink"
    if not command("docker", "network", "ls", "-q", "--filter", "name=^" + network + "$"):
        command("docker", "network", "create", "--label", "nvke-nais-dpu-sim.run=" + sim.config["run_id"],
                "--subnet", "172.28.236.0/24", network)
    for pair, host in enumerate(sim.hosts):
        if host.get("simulation_links_ready"):
            continue
        for node, role in ((host["container"], "host"), (host["dpu"], "dpu")):
            execute = sim.exec if role == "host" else sim.dpu_exec
            seed_name = host["name"] if role == "host" else host["dpu"]
            digest = hashlib.sha256((seed_name + "\0" + role).encode()).digest()
            links = json.loads(execute(node, "ip", "-j", "link", "show"))
            for link in links:
                identity = link.get("ifalias") or link["ifname"]
                prefix = "eth0-" if role == "host" else "rep0-"
                if identity.startswith(prefix):
                    index = int(identity[len(prefix):])
                    mac = f"52:54:00:{digest[0]:02x}:{digest[1]:02x}:{index:02x}"
                    execute(node, "ip", "link", "set", link["ifname"], "address", mac)
        sim.exec(host["container"], "ip", "address", "replace", f"172.28.236.{20 + pair}/24", "dev", "eth0-3")
        details = json.loads(command("docker", "inspect", host["dpu"]))[0]
        if network not in details["NetworkSettings"]["Networks"]:
            command("docker", "network", "connect", network, host["dpu"])
            sim.exec(host["dpu"], "ip", "link", "set", "eth2", "netns", OFFLOAD_NAMESPACE)
        details = json.loads(command("docker", "inspect", host["dpu"]))[0]
        address = details["NetworkSettings"]["Networks"][network]["IPAddress"]
        sim.dpu_exec(host["dpu"], "sh", "-ec",
                  "ovs-vsctl --may-exist add-br breth-uplink; "
                  "ovs-vsctl --may-exist add-port breth-uplink eth2 -- --may-exist add-port breth-uplink rep0-3; "
                  "ovs-vsctl br-set-external-id breth-uplink bridge-uplink eth2; "
                  "ip address flush dev eth2; ip link set eth2 up; ip link set breth-uplink up; "
                  'ip address replace "$1/24" dev breth-uplink', "uplink", address)
        host["simulation_links_ready"] = True
        sim.save()
    print("Configured deterministic simulated MACs and reserved host uplinks", flush=True)
