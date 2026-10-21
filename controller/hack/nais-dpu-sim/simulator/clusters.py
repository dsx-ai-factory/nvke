# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Build cluster requests and inspect NVKE-provisioned nodes."""
import json

import yaml

from .config import INPUTS, IMAGE, WORKLOAD_ROLE
from .runtime import command, wait_until, free_port


def provision(sim, role):
    """Request cluster creation through the NVKE API and save its ID without waiting for provisioning to finish."""
    request = cluster_request(sim, role)
    existing = next((c for c in sim.nvke("GET", "/clusters")["clusters"] if c["name"] == request["name"]), None)
    if existing and existing.get("conditions", {}).get("deleting", {}).get("status") in [True, "True", "true"]:
        wait_until("previous cluster deletion completed", lambda: not any(
            c["name"] == request["name"] for c in sim.nvke("GET", "/clusters")["clusters"]), 600)
    (sim.state / (role + "-request.json")).write_text(json.dumps(request, indent=2))
    cluster = sim.nvke("POST", "/clusters", request)
    sim.config["cluster_ids"][role] = cluster["id"]
    sim.save()
    print(f"NVKE accepted {role} cluster {cluster['id']}", flush=True)


def cluster_request(sim, role):
    name = sim.name + "-" + role
    ids = sim.config["ids"]
    pools = []
    for control_plane, prefix in [(True, "cp"), (False, "w")]:
        pools.append({"name": "control-plane" if control_plane else "workers", "nodeNamePrefix": prefix,
            "systemReservedMemory": "256Mi", "kubeReservedMemory": "256Mi", "desiredNodeCount": 1,
            "controlPlane": control_plane, "autoProvisionDisabled": False, "autoModeDisabled": False,
            "recycleBudget": 0, "teleportEnabled": False,
            "infra": {"machineTemplate": {"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1", "kind": "NicoMachineTemplate",
                "metadata": {"name": name + "-" + prefix}, "spec": {"template": {"spec": {
                    "vpcID": ids["2"], "instanceTypeID": ids["5"], "cloudInitInjectHostname": True,
                    "ipxeScript": "#!ipxe\necho simulated-managed-node\n", "sshKeyGroupIDs": [ids["4"]],
                    "interfaces": [{"subnetID": ids["3"]}]}}}},
                "preKubeadmCommands": ["mkdir -p /run/cluster-api", "echo success > /run/cluster-api/bootstrap-success.complete"]}})
    if role == "dpu":
        for pool in pools:
            pool["infra"]["preKubeadmCommands"].append("install -m 0644 /etc/dgx/ovn-kubernetes/values-dpu-primary.yaml /etc/dgx/ovn-kubernetes/values-nvke.yaml")
    else:
        pools[1]["nodeLabels"] = {"nodeType": "gpu"}
    request = {"name": name, "controlPlaneKubernetesVersion": "1.34.8", "etcdEncrypted": False,
        "cniPlugin": "ovn-kubernetes" if role == "dpu" else "ovn-kubernetes-dpu-host", "nodePools": pools[1:],
        "controlPlaneConfig": {"desiredCount": 1, "systemReservedMemory": "256Mi",
                               "teleportEnabled": False, "infra": pools[0]["infra"]},
        "cniVersion": INPUTS["ovn"]["version"],
        "infra": {"cluster": {"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1", "kind": "NicoCluster",
            "metadata": {"name": name}, "spec": {"siteID": ids["1"]}}}}
    if role != "dpu":
        request["network"] = {"podCIDR": "10.234.64.0/18", "serviceCIDR": "10.234.0.0/18"}
    else:
        request["network"] = {"podCIDR": "10.236.64.0/18", "serviceCIDR": "10.236.0.0/18"}
    return request


def discover(sim, role):
    """Match NVKE nodes for the selected cluster role to running containers and return their IDs and IPs, with control-plane nodes first."""
    cluster_id = sim.config["cluster_ids"][role]
    nodes = sim.nvke("GET", "/nodes?clusterID=" + cluster_id)["nodes"]
    containers = command("docker", "ps", "-q", "--filter", "label=nvidia.com/nico-containers.site-id=" + sim.config["ids"]["1"]).split()
    details = json.loads(command("docker", "inspect", *containers)) if containers else []
    discovered = []
    for node in nodes:
        container = next((d for d in details if d["Config"]["Hostname"] == node["name"]), None)
        if not container:
            continue
        network = container["NetworkSettings"]["Networks"].get(sim.name)
        if not network:
            continue
        discovered.append({"name": node["name"], "node_id": node["id"], "container": container["Id"],
            "id": container["Config"]["Labels"]["nvidia.com/nico-containers.instance-id"], "ip": network["IPAddress"]})
    return sorted(discovered, key=lambda n: ("-cp-" not in n["name"], n["name"]))


def ready(sim, role):
    cluster = sim.nvke("GET", "/clusters/" + sim.config["cluster_ids"][role])
    condition = cluster.get("conditions", {}).get("provisioned", {})
    return condition.get("status") in [True, "True", "true"]


def nodes_ready(sim, role):
    kubectl = sim.dpu_kubectl if role == "dpu" else sim.kubectl
    nodes = json.loads(kubectl("get", "nodes", "-o", "json"))["items"]
    return len(nodes) == 1 + sim.config.get("workers", 1) and all(any(c["type"] == "Ready" and c["status"] == "True"
                                     for c in n["status"]["conditions"]) for n in nodes)


def export_kubeconfig(sim, role, nodes):
    raw = sim.exec(nodes[0]["container"], "cat", "/etc/kubernetes/admin.conf")
    port = sim.config.setdefault("api_ports", {}).setdefault(role, free_port())
    proxy = sim.name + "-" + role + "-api"
    relay = """import select,socket,threading,sys
target=sys.argv[1]
def serve(client):
 try:
  remote=socket.create_connection((target,6443),10)
  with client,remote:
   while True:
    for source in select.select([client,remote],[],[],60)[0]:
     data=source.recv(65536)
     if not data:return
     (remote if source is client else client).sendall(data)
 except OSError:client.close()
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('0.0.0.0',6443));s.listen()
while True:threading.Thread(target=serve,args=(s.accept()[0],),daemon=True).start()
"""
    if not command("docker", "ps", "-q", "--filter", "name=^/" + proxy + "$"):
        command("docker", "run", "-d", "--name", proxy, "--network", sim.name,
                "--label", "nvke-nais-dpu-sim.run=" + sim.config["run_id"], "-p", f"127.0.0.1:{port}:6443",
                "--entrypoint", "python3", IMAGE, "-c", relay, nodes[0]["ip"])
    cfg = yaml.safe_load(raw)
    for cluster in cfg["clusters"]:
        cluster["cluster"]["server"] = f"https://127.0.0.1:{port}"
        cluster["cluster"]["tls-server-name"] = nodes[0]["ip"]
    path = sim.state / (role + ".kubeconfig")
    path.write_text(yaml.safe_dump(cfg))
    path.chmod(0o600)
    sim.save()
