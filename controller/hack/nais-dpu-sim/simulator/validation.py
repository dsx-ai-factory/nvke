# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Assert provisioning, network behavior, and runtime isolation."""
import json
import re


from .config import INPUTS, IMAGE, WORKLOAD_ROLE
from .runtime import command, require, wait_until
from .workloads import HTTP_SERVER
from .dpu_workload_network import KUBE_NAMESPACE as OFFLOAD_KUBE_NAMESPACE, RUN_DIR as OFFLOAD_RUN_DIR
from . import clusters, traffic


def verify(sim):
    """Check provisioning and OVN state, then exercise traffic in both clusters."""
    verify_host_provisioning(sim)
    verify_ovn_runtime(sim)
    traffic.verify(sim)
    verify_dpu_cni(sim)


def verify_ovn_runtime(sim):
    """Require the pinned OVN image and ready host, workload-DPU, and primary-DPU components."""
    image_id = INPUTS["ovn"]["config_digest"]
    require(command("docker", "image", "inspect", IMAGE, "--format", "{{.Id}}") == image_id, "The local upstream image changed")
    for node in [h["container"] for h in sim.hosts] + [h["dpu"] for h in sim.hosts]:
        installed = json.loads(sim.exec(node, "crictl", "inspecti", IMAGE))
        require(installed["status"]["id"] == image_id, "Node has a stale simulation image: " + node)
    print("PASS all host and DPU runtimes contain the pinned upstream image", flush=True)
    for kubectl, daemon, namespace in ((sim.kubectl, "ovnkube-node-dpu-host", "ovn-kubernetes"),
                                       (sim.dpu_kubectl, "ovnkube-node-dpu", OFFLOAD_KUBE_NAMESPACE),
                                       (sim.dpu_kubectl, "ovnkube-node", "ovn-kubernetes")):
        kubectl("rollout", "status", "daemonset/" + daemon, "-n", namespace, "--timeout=300s")
        daemon_status = json.loads(kubectl("get", "daemonset/" + daemon, "-n", namespace, "-o", "json"))["status"]
        require(daemon_status["desiredNumberScheduled"] == len(sim.hosts) and daemon_status["numberReady"] == len(sim.hosts),
                f"{namespace}/{daemon} must run on every requested node")
        running = json.loads(kubectl("get", "pods", "-n", namespace, "-o", "json"))["items"]
        images = [c for pod in running for c in pod["status"].get("containerStatuses", [])
                  if c["image"].endswith(IMAGE)]
        require(len(images) >= len(sim.hosts), "Expected upstream OVN containers on every node")
        require(all(c["imageID"] == image_id and "running" in c["state"] for c in images),
                "OVN must be running the pinned upstream image")
    print("PASS host and DPU OVN containers run the pinned upstream image", flush=True)
    (sim.state / "ovn-runtime-validation.json").write_text(json.dumps({
        "image": IMAGE, "image_id": image_id, "upstream_revision": INPUTS["ovn"]["revision"],
    }, indent=2))


def verify_host_provisioning(sim):
    nodes = {n["metadata"]["name"]: n for n in json.loads(sim.kubectl("get", "nodes", "-o", "json"))["items"]}
    report = []
    for host in sim.hosts:
        node = nodes[host["name"]]
        expected = {"k8s.ovn.org/dpu-host": ""}
        if "-cp-" not in host["name"]:
            expected["nodeType"] = "gpu"
        flags = sim.exec(host["container"], "cat", "/var/lib/kubelet/kubeadm-flags.env")
        match = re.search(r'--node-labels=([^\s"]+)', flags)
        require(match is not None, "Kubeadm did not configure first-registration node labels")
        labels = dict(label.split("=", 1) for label in match[1].split(","))
        for key, value in expected.items():
            require(labels.get(key) == value, f"Missing kubelet bootstrap label {key} on {host['name']}")
            require(node["metadata"]["labels"].get(key) == value, f"Missing live node label {key} on {host['name']}")
        links = json.loads(sim.exec(host["container"], "ip", "-j", "-d", "link", "show"))
        require(not any(link.get("linkinfo", {}).get("info_kind") == "openvswitch" for link in links),
                "A DPU host has a local OVS bridge")
        require(sim.management(host["container"]) == host["management"], "DPU-host management networking changed")
        require(not node["spec"].get("unschedulable"), "DPU host is cordoned")
        ipv6 = {scope: sim.exec(host["container"], "cat", f"/proc/sys/net/ipv6/conf/{scope}/disable_ipv6")
                for scope in ["all", "default", "lo"]}
        require(all(value == "0" for value in ipv6.values()), "DPU-host IPv6 is disabled")
        if "-cp-" not in host["name"]:
            pod = "gpu-worker-" + host["node_id"][:8]
            sim.apply([{"apiVersion": "v1", "kind": "Pod", "metadata": {"name": pod},
                "spec": {"nodeSelector": {"nodeType": "gpu", "kubernetes.io/hostname": host["name"]},
                         "containers": [{"name": "http", "image": IMAGE, "imagePullPolicy": "IfNotPresent",
                             "command": ["python3", "-c", HTTP_SERVER], "resources": {"limits": {"dpusim.io/vf": "1"}},
                             "readinessProbe": {"httpGet": {"path": "/", "port": 8080}, "periodSeconds": 2}}]}}])
            sim.kubectl("wait", "pod/" + pod, "--for=condition=Ready", "--timeout=180s")
            scheduled = sim.kubectl("get", "pod/" + pod, "-o", "jsonpath={.spec.nodeName}")
            require(scheduled == host["name"], "GPU selector did not schedule onto the intended worker")
        report.append({"name": host["name"], "bootstrap_labels": labels, "disable_ipv6": ipv6,
                       "management": host["management"], "ovs_bridge": False, "schedulable": True})
    (sim.state / "host-provisioning-validation.json").write_text(json.dumps(report, indent=2))
    print("PASS DPU-host kubelet labels, no host OVS bridge, IPv6 and schedulability", flush=True)


def verify_dpu_cni(sim):
    cluster = sim.nvke("GET", "/clusters/" + sim.config["cluster_ids"]["dpu"])
    require(cluster["cniPlugin"] == "ovn-kubernetes", "The DPU cluster must be provisioned with OVN as its primary CNI")
    daemons = json.loads(sim.dpu_kubectl("get", "daemonsets", "-A", "-o", "json"))["items"]
    require(not any(any(cni in d["metadata"]["name"] for cni in ["calico", "flannel", "kube-proxy"]) for d in daemons),
            "A previous primary CNI or kube-proxy remains in the DPU cluster")
    report = {"cluster": cluster["name"], "cniPlugin": cluster["cniPlugin"], "nodes": []}
    for node in sim.dpus:
        primary = sim.exec(node["container"], "cat", "/etc/cni/net.d/10-ovn-kubernetes.conf")
        require(json.loads(primary)["type"] == "ovn-k8s-cni-overlay", "The DPU kubelet does not delegate to OVN")
        root_ports = sim.exec(node["container"], "ovs-vsctl", "list-ports", "br-int").splitlines()
        offload_ports = sim.dpu_exec(node["container"], "ovs-vsctl", "list-ports", "br-int").splitlines()
        require(not any(p.startswith("rep0-") for p in root_ports), "Primary OVN owns a secondary VF representor")
        require(any(p.startswith("rep0-") for p in offload_ports), "The workload network has no secondary pod representor")
        script = """import json,os,socket
from pathlib import Path
s=socket.socket(socket.AF_INET6,socket.SOCK_STREAM);s.bind(('::1',0));s.close()
print(json.dumps({'netns_inode':os.stat('/proc/self/ns/net').st_ino,
    'disable_ipv6':{n:(Path('/proc/sys/net/ipv6/conf')/n/'disable_ipv6').read_text().strip() for n in ['all','default','lo']}}))"""
        root = json.loads(sim.exec(node["container"], "python3", "-c", script))
        offload = json.loads(sim.dpu_exec(node["container"], "python3", "-c", script))
        require(root["netns_inode"] != offload["netns_inode"], "Primary and offload OVN share a network namespace")
        require(all(v == "0" for state in [root, offload] for v in state["disable_ipv6"].values()), "IPv6 is disabled for FRR's sockets")
        paths = ["/var/run/openvswitch/db.sock", OFFLOAD_RUN_DIR + "/db.sock"]
        inodes = json.loads(sim.exec(node["container"], "python3", "-c",
                                     "import json,os,sys; print(json.dumps([os.stat(p).st_ino for p in sys.argv[1:]]))", *paths))
        require(inodes[0] != inodes[1], "Primary and offload OVN share an OVS database socket")
        databases = {}
        for filename in ["ovnnb_db.db", "ovnsb_db.db"]:
            db_paths = ["/var/lib/openvswitch/" + filename, "/var/lib/nvke-nais-dpu-sim-offload/" + filename]
            db_inodes = json.loads(sim.exec(node["container"], "python3", "-c",
                "import json,os,sys; print(json.dumps([os.stat(p).st_ino for p in sys.argv[1:]]))", *db_paths))
            require(db_inodes[0] != db_inodes[1], "Primary and offload OVN share a database file")
            databases[filename] = db_paths
        report["nodes"].append({"name": node["name"], "primary": root, "offload": offload,
                                "ovs_socket_paths": paths, "ovn_database_paths": databases,
                                "primary_ports": root_ports, "offload_ports": offload_ports})
    (sim.state / "dpu-primary-validation.json").write_text(json.dumps(report, indent=2))
    print("PASS DPU primary CNI is OVN; primary/offload namespaces, OVS databases and representors are isolated; IPv6 sockets work", flush=True)


def verify_management(sim):
    records = {role: sim.nvke("GET", "/clusters/" + cid) for role, cid in sim.config["cluster_ids"].items()}
    require(all(clusters.ready(sim, role) for role in records), "NVKE has not marked both clusters provisioned")
    require(clusters.nodes_ready(sim, "dpu") and clusters.nodes_ready(sim, WORKLOAD_ROLE), "Expected all requested nodes Ready in each tenant cluster")
    expected = {"provision-cluster-" + cid for cid in sim.config["cluster_ids"].values()}
    expected.update("provision-node-" + n["node_id"] for n in sim.dpus + sim.hosts)
    def completed_workflows():
        runs = json.loads(sim.mgmt("exec", "-n", "nvke-nais-dpu-sim", "deployment/temporal", "--", "temporal",
                                   "workflow", "list", "--address", "temporal:7233", "--output", "json"))
        selected = {r["execution"]["workflowId"]: r for r in runs if r["execution"]["workflowId"] in expected}
        return selected if len(selected) == len(expected) and all(r["status"] == "WORKFLOW_EXECUTION_STATUS_COMPLETED"
                                                                  for r in selected.values()) else None
    workflows = wait_until("both cluster and all node provisioning workflows completed", completed_workflows, 180)
    report = {"management_cluster": sim.config["management_cluster"], "docker_context": sim.config["docker_context"],
              "clusters": {role: {"id": c["id"], "name": c["name"], "cniPlugin": c["cniPlugin"],
                                  "cniVersion": c["cniVersion"],
                                  "conditions": c.get("conditions")} for role, c in records.items()},
              "dpu_nodes": [{k: n[k] for k in ["name", "node_id", "id", "ip"]} for n in sim.dpus],
              "workload_nodes": [{k: n[k] for k in ["name", "node_id", "id", "ip", "dpu_node_name"]} for n in sim.hosts],
              "workflows": [{"id": key, "run_id": r["execution"]["runId"], "status": r["status"]} for key, r in workflows.items()]}
    (sim.state / "management-validation.json").write_text(json.dumps(report, indent=2))
    print("PASS NVKE management API reports both clusters provisioned; NICo instances and host/DPU pairing recorded", flush=True)
