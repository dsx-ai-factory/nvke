#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Provision the DPU and VMaaS clusters through an isolated NKE management cluster."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

import yaml

from lab import ROOT, STATE, command, require, chart_check
from networking import NetworkingFixtures, IMAGE, SIM_IMAGE, PLUGIN_IMAGE, HTTP_SERVER
from offload import setup as setup_offload, KUBE_NAMESPACE as OFFLOAD_KUBE_NAMESPACE
from offload import RUN_DIR as OFFLOAD_RUN_DIR

NODE_IMAGE = "nke-dpu-managed-node:local"
RUNTIME_IMAGE = "nke-dpu-managed-runtime:local"
KIND_IMAGE = "kindest/node:v1.34.8"
PROVIDER_IMAGE = "cluster-api-provider-nico:local"
TEMPORAL_IMAGE = "temporalio/temporal:1.9.1"
POSTGRES_IMAGE = "postgres:17.4"


def wait_until(description, check, timeout=900):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (RuntimeError, OSError, urllib.error.URLError) as err:
            last = str(err)
        time.sleep(3)
    raise RuntimeError(f"Timed out waiting for {description}: {last or 'not ready'}")


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def service(name, port, node_port=None):
    spec = {"selector": {"app": name}, "ports": [{"port": port, "targetPort": port}]}
    if node_port:
        spec["type"] = "NodePort"
        spec["ports"][0]["nodePort"] = node_port
    return {"apiVersion": "v1", "kind": "Service", "metadata": {"name": name, "namespace": "cloud-workflow-engine"}, "spec": spec}


def deployment(name, image, env=None, args=None, account=None, data_path=None, port=None):
    container = {"name": name, "image": image, "imagePullPolicy": "IfNotPresent", "env": env or []}
    if args:
        container["args"] = args
    if port:
        container["readinessProbe"] = {"tcpSocket": {"port": port}, "initialDelaySeconds": 5, "periodSeconds": 3}
    pod = {"containers": [container], "tolerations": [{"operator": "Exists"}]}
    if account:
        pod["serviceAccountName"] = account
    if data_path:
        mount = "/var/lib/postgresql/data" if name == "postgres" else "/data"
        container["volumeMounts"] = [{"name": "data", "mountPath": mount}]
        pod["volumes"] = [{"name": "data", "hostPath": {"path": data_path, "type": "DirectoryOrCreate"}}]
    return {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": name, "namespace": "cloud-workflow-engine"},
            "spec": {"replicas": 1, "selector": {"matchLabels": {"app": name}},
                     "template": {"metadata": {"labels": {"app": name}}, "spec": pod}}}


class ManagedLab(NetworkingFixtures):
    def __init__(self, state):
        super().__init__(state)
        os.environ["DOCKER_CONTEXT"] = self.config["docker_context"]
        os.environ.pop("DOCKER_HOST", None)
        self.dpus = self.config.setdefault("dpu_nodes", [])

    def mgmt(self, *args, **kwargs):
        return command("kubectl", "--kubeconfig", str(self.state / "management.kubeconfig"), *args, **kwargs)

    def dpu_kubectl(self, *args, **kwargs):
        require(bool(self.dpus), "DPU nodes have not been provisioned")
        return self.exec(self.dpus[0]["container"], "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", *args, **kwargs)

    def nke(self, method, path, payload=None):
        request = urllib.request.Request(self.config["controller_api"] + path, method=method,
                                        data=json.dumps(payload).encode() if payload is not None else None,
                                        headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                data = response.read()
                return json.loads(data) if data else None
        except urllib.error.HTTPError as err:
            raise RuntimeError(f"NKE {method} {path}: HTTP {err.code}: {err.read().decode()}") from err

    def management_up(self, provider_chart):
        kubeconfig = self.state / "management.kubeconfig"
        kind_config = {"kind": "Cluster", "apiVersion": "kind.x-k8s.io/v1alpha4",
                       "networking": {"podSubnet": "10.237.0.0/16", "serviceSubnet": "10.238.0.0/16"},
                       "nodes": [{"role": "control-plane", "extraPortMappings": [
                           {"containerPort": 30080, "hostPort": self.config["controller_port"], "listenAddress": "127.0.0.1"}]}]}
        (self.state / "management-kind.yaml").write_text(yaml.safe_dump(kind_config))
        manager = self.config["management_cluster"] + "-control-plane"
        if not command("docker", "ps", "-q", "--filter", "name=^/" + manager + "$"):
            command("kind", "create", "cluster", "--name", self.config["management_cluster"], "--image", KIND_IMAGE,
                    "--config", str(self.state / "management-kind.yaml"), "--kubeconfig", str(kubeconfig),
                    env={**os.environ, "KIND_EXPERIMENTAL_DOCKER_NETWORK": self.name})
        kubeconfig.chmod(0o600)
        archive = self.state / "management-images.tar"
        command("docker", "save", "-o", str(archive), RUNTIME_IMAGE, PROVIDER_IMAGE, TEMPORAL_IMAGE, POSTGRES_IMAGE)
        command("docker", "cp", str(archive), manager + ":/var/lib/management-images.tar")
        self.exec(manager, "ctr", "-n", "k8s.io", "images", "import", "/var/lib/management-images.tar")
        self.exec(manager, "rm", "/var/lib/management-images.tar")
        command("clusterctl", "init", "--core", "cluster-api:v1.12.3", "--bootstrap", "kubeadm:v1.12.3",
                "--control-plane", "kubeadm:v1.12.3", "--kubeconfig", str(kubeconfig))
        command("helm", "upgrade", "--install", "capi-provider-nico", str(provider_chart), "--kubeconfig", str(kubeconfig),
                "--namespace", "capnico-system", "--create-namespace", "--set-string", "manager.image.repository=cluster-api-provider-nico",
                "--set-string", "manager.image.tag=local", "--wait", "--timeout", "5m")
        self.mgmt("apply", "-f", "-", input=yaml.safe_dump({"apiVersion": "v1", "kind": "Secret",
            "metadata": {"name": "nico-credentials", "namespace": "capnico-system"},
            "stringData": {"endpoint": self.config["nico_endpoint"], "orgID": "novxwn552ssc", "apiName": "forge", "token": "example-token"}}))
        ns = {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "cloud-workflow-engine"}}
        self.mgmt("apply", "-f", "-", input=yaml.safe_dump(ns))
        self.mgmt("apply", "-f", "-", input=yaml.safe_dump({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "nke-system"}}))
        password = secrets.token_urlsafe(32)
        db_secret = {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "lab-db", "namespace": "cloud-workflow-engine"},
                     "stringData": {"password": password}}
        try:
            self.mgmt("get", "secret/lab-db", "-n", "cloud-workflow-engine", "-o", "name")
            db_secret = None
        except RuntimeError as err:
            if "NotFound" not in str(err):
                raise
        password_ref = {"secretKeyRef": {"name": "lab-db", "key": "password"}}
        account = {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "nke-controller", "namespace": "cloud-workflow-engine"}}
        binding = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": {"name": "nke-dpu-lab-controller"},
                   "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "cluster-admin"},
                   "subjects": [{"kind": "ServiceAccount", "name": "nke-controller", "namespace": "cloud-workflow-engine"}]}
        postgres = deployment("postgres", POSTGRES_IMAGE,
            [{"name": "POSTGRES_DB", "value": "nke"}, {"name": "POSTGRES_PASSWORD", "valueFrom": password_ref}],
            data_path="/var/local/nke-dpu-lab/postgres", port=5432)
        temporal = deployment("temporal", TEMPORAL_IMAGE, args=["server", "start-dev", "--ip", "0.0.0.0", "--db-filename", "/data/temporal.db"],
                              data_path="/var/local/nke-dpu-lab/temporal", port=7233)
        # hostPath directories are created by root; the dev Temporal image runs
        # as a non-root user and needs a writable SQLite directory.
        self.exec(manager, "mkdir", "-p", "/var/local/nke-dpu-lab/temporal")
        self.exec(manager, "chown", "1000:1000", "/var/local/nke-dpu-lab/temporal")
        env = {"DB_CONNECTION_STRING": "postgres://postgres:REDACTED@postgres:5432/nke?sslmode=disable",
               "TEMPORAL_HOST_PORT": "temporal:7233", "TEMPORAL_NAMESPACE": "default", "SPIFFE_ENABLED": "false",
               "INSTALL_CNI_AFTER_INIT": "true", "NODE_AGENT_PORT": "15005", "NODE_HEALTH_CHECK_DISABLED": "false",
               "LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP": "true",
               "SUPPORTED_INFRA_PROVIDERS": "infra", "NICO_CONTROLLERS_ENABLED": "false",
               "PROFILES_DIR": "/profiles", "LOG_LEVEL": "info", "KUBEADM_IMAGE_REPOSITORY": "registry.k8s.io"}
        controller_env = [{"name": k, "value": v} for k, v in env.items()]
        controller_env.append({"name": "CNPG_SUPERUSER_PASSWORD", "valueFrom": password_ref})
        controller = deployment("nke-controller", RUNTIME_IMAGE, controller_env, account="nke-controller", port=8080)
        controller["spec"]["template"]["spec"]["containers"][0]["command"] = ["controller"]
        objects = [account, binding, postgres, temporal, controller,
                   service("postgres", 5432), service("temporal", 7233), service("nke-controller", 8080, 30080)]
        if db_secret:
            objects.insert(0, db_secret)
        self.mgmt("apply", "-f", "-", input=yaml.safe_dump_all(objects))
        self.mgmt("rollout", "status", "deployment/nke-controller", "-n", "cloud-workflow-engine", "--timeout=300s")
        wait_until("NKE REST API", lambda: self.nke("GET", "/version"), 120)
        self.config["management_ready"] = True
        self.save()
        print("NKE controller, Temporal, PostgreSQL, CAPI and CAPNICo are running in the management cluster", flush=True)

    def cluster_request(self, role):
        name = self.name + "-" + role
        ids = self.config["ids"]
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
                pool["infra"]["preKubeadmCommands"].append("install -m 0644 /etc/dgx/ovn-kubernetes/values-dpu-primary.yaml /etc/dgx/ovn-kubernetes/values-nke.yaml")
        else:
            pools[1]["nodeLabels"] = ["nodeType=gpu"]
        request = {"name": name, "controlPlaneKubernetesVersion": "1.34.8", "etcdEncrypted": False,
            "cniPlugin": "ovn-kubernetes" if role == "dpu" else "ovn-kubernetes-dpu-host", "nodePools": pools,
            "cniVersion": "1.5.18",
            "infra": {"cluster": {"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1", "kind": "NicoCluster",
                "metadata": {"name": name}, "spec": {"siteID": ids["1"]}}}}
        if role != "dpu":
            request["network"] = {"podCIDR": "10.234.64.0/18", "serviceCIDR": "10.234.0.0/18"}
        else:
            request["network"] = {"podCIDR": "10.236.64.0/18", "serviceCIDR": "10.236.0.0/18"}
        return request

    def provision(self, role):
        request = self.cluster_request(role)
        existing = next((c for c in self.nke("GET", "/clusters")["clusters"] if c["name"] == request["name"]), None)
        if existing and existing.get("conditions", {}).get("deleting", {}).get("status") in [True, "True", "true"]:
            wait_until("previous cluster deletion completed", lambda: not any(
                c["name"] == request["name"] for c in self.nke("GET", "/clusters")["clusters"]), 600)
        (self.state / (role + "-request.json")).write_text(json.dumps(request, indent=2))
        cluster = self.nke("POST", "/clusters", request)
        self.config["cluster_ids"][role] = cluster["id"]
        self.save()
        print(f"NKE accepted {role} cluster {cluster['id']}", flush=True)

    def discover(self, role):
        cluster_id = self.config["cluster_ids"][role]
        nodes = self.nke("GET", "/nodes?clusterID=" + cluster_id)["nodes"]
        containers = command("docker", "ps", "-q", "--filter", "label=nvidia.com/nico-containers.site-id=" + self.config["ids"]["1"]).split()
        details = json.loads(command("docker", "inspect", *containers)) if containers else []
        discovered = []
        for node in nodes:
            container = next((d for d in details if d["Config"]["Hostname"] == node["name"]), None)
            if not container:
                continue
            network = container["NetworkSettings"]["Networks"].get(self.name)
            if not network:
                continue
            discovered.append({"name": node["name"], "node_id": node["id"], "container": container["Id"],
                "id": container["Config"]["Labels"]["nvidia.com/nico-containers.instance-id"], "ip": network["IPAddress"]})
        return sorted(discovered, key=lambda n: ("-cp-" not in n["name"], n["name"]))

    def ready(self, role):
        cluster = self.nke("GET", "/clusters/" + self.config["cluster_ids"][role])
        condition = cluster.get("conditions", {}).get("provisioned", {})
        return condition.get("status") in [True, "True", "true"]

    def nodes_ready(self, role):
        kubectl = self.dpu_kubectl if role == "dpu" else self.kubectl
        nodes = json.loads(kubectl("get", "nodes", "-o", "json"))["items"]
        return len(nodes) == 1 + self.config.get("workers", 1) and all(any(c["type"] == "Ready" and c["status"] == "True"
                                         for c in n["status"]["conditions"]) for n in nodes)

    def export_kubeconfig(self, role, nodes):
        raw = self.exec(nodes[0]["container"], "cat", "/etc/kubernetes/admin.conf")
        port = self.config.setdefault("api_ports", {}).setdefault(role, free_port())
        proxy = self.name + "-" + role + "-api"
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
            command("docker", "run", "-d", "--name", proxy, "--network", self.name,
                    "--label", "nke-dpu-lab.run=" + self.config["run_id"], "-p", f"127.0.0.1:{port}:6443",
                    "--entrypoint", "python3", SIM_IMAGE, "-c", relay, nodes[0]["ip"])
        cfg = yaml.safe_load(raw)
        for cluster in cfg["clusters"]:
            cluster["cluster"]["server"] = f"https://127.0.0.1:{port}"
            cluster["cluster"]["tls-server-name"] = nodes[0]["ip"]
        path = self.state / (role + ".kubeconfig")
        path.write_text(yaml.safe_dump(cfg))
        path.chmod(0o600)
        self.save()

    def provision_dpu(self):
        if "dpu" not in self.config["cluster_ids"]:
            self.provision("dpu")
        count = 1 + self.config.get("workers", 1)
        discovered = wait_until("NKE-provisioned DPU nodes", lambda: (n if len(n := self.discover("dpu")) == count else None))
        previous = {n["node_id"]: n for n in self.dpus}
        # Keep fixture addresses stable when a new worker sorts before an old one.
        by_id = {n["node_id"]: n for n in discovered}
        self.dpus[:] = [{**old, **by_id[old["node_id"]]} for old in self.dpus if old["node_id"] in by_id]
        self.dpus.extend(n for n in discovered if n["node_id"] not in previous)
        self.save()
        wait_until("NKE DPU cluster provisioned", lambda: self.ready("dpu"))
        wait_until("DPU Kubernetes nodes Ready", lambda: self.nodes_ready("dpu"), 300)
        self.dpu_kubectl("rollout", "status", "daemonset/ovnkube-node", "-n", "ovn-kubernetes", "--timeout=300s")
        setup_offload(self)
        self.export_kubeconfig("dpu", self.dpus)
        print(f"PASS DPU cluster provisioned by NKE and all {count} nodes Ready", flush=True)

    def provision_secondary(self, record_success=True):
        if "vmaas" not in self.config["cluster_ids"]:
            self.provision("vmaas")
        deadline = time.monotonic() + 1200
        while time.monotonic() < deadline:
            nodes = self.discover("vmaas")
            # The first host supplies admin.conf and API access for the fixtures.
            if not self.hosts and not any("-cp-" in node["name"] for node in nodes):
                time.sleep(3)
                continue
            added = False
            for node in nodes:
                if any(h["node_id"] == node["node_id"] for h in self.hosts):
                    continue
                used = {host["dpu"] for host in self.hosts}
                dpu = next(d for d in self.dpus if d["container"] not in used)
                node["dpu"] = dpu["container"]
                node["dpu_node_name"] = dpu["name"]
                node["management"] = self.management(node["container"])
                self.hosts.append(node)
                self.save()
                added = True
            if self.hosts and (added or any(not h.get("fixtures_ready") for h in self.hosts)):
                host_ids = sorted(h["node_id"] for h in self.hosts)
                if self.config.get("installed_host_ids") != host_ids:
                    wait_until("secondary API initialized by node agent", lambda: self.exec(self.hosts[0]["container"], "test", "-s", "/etc/kubernetes/admin.conf") is not None)
                    self.wire()
                    self.load()
                    self.simulation_links()
                    self.load_helpers()
                    node = json.loads(self.kubectl("get", "node", self.hosts[0]["name"], "-o", "json"))
                    if any(t["key"] == "node-role.kubernetes.io/control-plane" for t in node["spec"].get("taints", [])):
                        self.kubectl("taint", "node", self.hosts[0]["name"], "node-role.kubernetes.io/control-plane-")
                    self.install()
                    self.config["installed_host_ids"] = host_ids
                    self.save()
                self.routing()
                for host in self.hosts:
                    host["fixtures_ready"] = True
                self.save()
            if self.hosts:
                self.configure_dns_devices()
            if len(self.hosts) == 1 + self.config.get("workers", 1) and self.ready("vmaas"):
                wait_until("secondary Kubernetes nodes Ready", lambda: self.nodes_ready("vmaas"), 300)
                self.export_kubeconfig("vmaas", self.hosts)
                self.verify()
                self.verify_management()
                if record_success:
                    self.mark_validated()
                return
            time.sleep(5)
        raise RuntimeError("NKE secondary cluster did not complete provisioning; inspect retained lab state")

    def scale(self, workers):
        require(1 <= workers <= 3, "This bounded lab supports one to three workers per cluster")
        require(workers >= self.config.get("workers", 1), "scale cannot decrease worker capacity")
        if workers > self.config.get("workers", 1):
            baseline = self.state / ("before-scale-" + str(workers))
            baseline.mkdir(mode=0o700, exist_ok=True)
            for name in ["management-validation.json", "network-matrix.json", "validation.json",
                         "dpu-primary-validation.json", "host-provisioning-validation.json"]:
                shutil.copy2(self.state / name, baseline / name)
        self.config["workers"] = workers
        self.config["validated"] = False
        self.save()
        for role in ["dpu", "vmaas"]:
            pools = self.nke("GET", "/nodePools?clusterID=" + self.config["cluster_ids"][role])["nodePools"]
            pool = next(p for p in pools if not p["controlPlane"])
            if role == "dpu":
                self.nke("PATCH", "/nodePools/" + pool["id"], {"desiredNodeCount": workers})
                print(f"Requested {workers} DPU workers through the NKE node-pool API", flush=True)
                self.provision_dpu()
            else:
                baselines = self.config.setdefault("scale_baselines", {})
                existing = set(baselines.setdefault(str(workers), [host["name"] for host in self.hosts]))
                versions = self.config.setdefault("scale_watch_versions", {})
                version = versions.setdefault(str(workers), json.loads(self.kubectl("get", "--raw", "/api/v1/nodes"))["metadata"]["resourceVersion"])
                require(version.isdigit() and int(version) > 0, "Node creation watch requires an API list revision")
                self.save()
                events = self.state / f"worker-registration-events-{workers}.json"
                with events.open("a") as output, (self.state / f"worker-registration-watch-{workers}.log").open("a") as errors:
                    watch = subprocess.Popen(["kubectl", "--kubeconfig", str(self.state / "vmaas.kubeconfig"),
                        "get", "--raw", "/api/v1/nodes?watch=true&resourceVersion=" + version,
                        "--request-timeout=900s"], stdout=output, stderr=errors)
                    try:
                        self.nke("PATCH", "/nodePools/" + pool["id"], {"desiredNodeCount": workers})
                        print(f"Requested {workers} secondary workers through the NKE node-pool API", flush=True)
                        self.provision_secondary(record_success=False)
                    finally:
                        watch.terminate()
                        watch.wait(timeout=10)
                decoder = json.JSONDecoder()
                pending = events.read_text().strip()
                registered = {}
                while pending:
                    event, end = decoder.raw_decode(pending)
                    pending = pending[end:].lstrip()
                    node = event["object"]
                    if event["type"] == "ADDED":
                        require(node["metadata"]["name"] not in existing, "Watch returned a pre-existing Node; cannot prove first registration")
                        registered.setdefault(node["metadata"]["name"], node["metadata"]["labels"])
                added = {host["name"] for host in self.hosts} - existing
                require(added and added <= registered.keys(), "Node watch did not capture all new workers' creation events")
                for name in added:
                    require(registered[name].get("k8s.ovn.org/dpu-host") == "" and registered[name].get("nodeType") == "gpu",
                            "New worker was missing required labels at first registration: " + name)
                (self.state / "worker-registration-validation.json").write_text(json.dumps(registered, indent=2))
                self.mark_validated()
                print("PASS new workers carried DPU-host and GPU labels on their initial Node creation events", flush=True)

    def configure_dns_devices(self):
        # NKE's DNS cache is a pod-network DaemonSet behind kube-dns. Both it
        # and CoreDNS need a VF from the simulation device plugin.
        for kind, name in [("daemonset", "node-local-dns"), ("deployment", "coredns")]:
            try:
                obj = json.loads(self.kubectl("get", kind + "/" + name, "-n", "kube-system", "-o", "json"))
            except RuntimeError as err:
                if "NotFound" in str(err):
                    continue
                raise
            for container in obj["spec"]["template"]["spec"]["containers"]:
                resources = container.setdefault("resources", {})
                if resources.get("limits", {}).get("dpusim.io/vf") == "1":
                    continue
                resources.setdefault("limits", {})["dpusim.io/vf"] = "1"
                resources.setdefault("requests", {})["dpusim.io/vf"] = "1"
                patch = {"spec": {"template": {"spec": {"containers": [{"name": container["name"], "resources": resources}]}}}}
                self.kubectl("patch", kind + "/" + name, "-n", "kube-system", "--type=strategic", "-p", json.dumps(patch))

    def load(self):
        # Managed node images preload these before the node agent starts.
        for node in [h["container"] for h in self.hosts] + [h["dpu"] for h in self.hosts]:
            wait_until("managed node image preload", lambda: self.exec(node, "systemctl", "is-active", "nke-dpu-preload.service") == "active", 600)
            for ref in [IMAGE, SIM_IMAGE, PLUGIN_IMAGE]:
                expected = command("docker", "image", "inspect", ref, "--format", "{{.Id}}")
                actual = json.loads(self.exec(node, "crictl", "inspecti", ref))["status"]["id"]
                require(actual == expected, "Managed node preload differs for " + ref)
        print("PASS simulation images preloaded by the managed node image", flush=True)

    def install_frr(self):
        # DaemonSets select Kubernetes node names, not Docker container IDs.
        saved = [(host, host["dpu"]) for host in self.hosts]
        for host, _ in saved:
            host["dpu"] = host["dpu_node_name"]
        try:
            super().install_frr()
        finally:
            for host, container in saved:
                host["dpu"] = container
        self.bootstrap_frr_webhook()

    def configure_uplink_rbac(self):
        role = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
                "metadata": {"name": "lab-ovn-uplink-finalizers"},
                "rules": [{"apiGroups": ["k8s.ovn.org"], "resources": ["uplinks/finalizers"], "verbs": ["update"],
                           "resourceNames": ["vm-uplink"]}]}
        binding = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
                   "metadata": {"name": "lab-ovn-uplink-finalizers"},
                   "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "lab-ovn-uplink-finalizers"},
                   "subjects": [{"kind": "ServiceAccount", "name": "ovn-node-dpu-host", "namespace": "ovn-kubernetes"}]}
        self.apply([role, binding])

    def install(self):
        self.configure_uplink_rbac()
        super().install()

    def verify(self):
        from matrix import verify_networks
        self.verify_host_provisioning()
        super().verify()
        verify_networks(self)
        self.verify_dpu_cni()

    def verify_host_provisioning(self):
        nodes = {n["metadata"]["name"]: n for n in json.loads(self.kubectl("get", "nodes", "-o", "json"))["items"]}
        report = []
        for host in self.hosts:
            node = nodes[host["name"]]
            expected = {"k8s.ovn.org/dpu-host": ""}
            if "-cp-" not in host["name"]:
                expected["nodeType"] = "gpu"
            flags = self.exec(host["container"], "cat", "/var/lib/kubelet/kubeadm-flags.env")
            match = re.search(r'--node-labels=([^\s"]+)', flags)
            require(match is not None, "Kubeadm did not configure first-registration node labels")
            labels = dict(label.split("=", 1) for label in match[1].split(","))
            for key, value in expected.items():
                require(labels.get(key) == value, f"Missing kubelet bootstrap label {key} on {host['name']}")
                require(node["metadata"]["labels"].get(key) == value, f"Missing live node label {key} on {host['name']}")
            links = json.loads(self.exec(host["container"], "ip", "-j", "-d", "link", "show"))
            require(not any(link.get("linkinfo", {}).get("info_kind") == "openvswitch" for link in links),
                    "A DPU host has a local OVS bridge")
            require(self.management(host["container"]) == host["management"], "DPU-host management networking changed")
            require(not node["spec"].get("unschedulable"), "DPU host is cordoned")
            ipv6 = {scope: self.exec(host["container"], "cat", f"/proc/sys/net/ipv6/conf/{scope}/disable_ipv6")
                    for scope in ["all", "default", "lo"]}
            require(all(value == "0" for value in ipv6.values()), "DPU-host IPv6 is disabled")
            if "-cp-" not in host["name"]:
                pod = "gpu-worker-" + host["node_id"][:8]
                self.apply([{"apiVersion": "v1", "kind": "Pod", "metadata": {"name": pod},
                    "spec": {"nodeSelector": {"nodeType": "gpu", "kubernetes.io/hostname": host["name"]},
                             "containers": [{"name": "http", "image": IMAGE, "imagePullPolicy": "IfNotPresent",
                                 "command": ["python3", "-c", HTTP_SERVER], "resources": {"limits": {"dpusim.io/vf": "1"}},
                                 "readinessProbe": {"httpGet": {"path": "/", "port": 8080}, "periodSeconds": 2}}]}}])
                self.kubectl("wait", "pod/" + pod, "--for=condition=Ready", "--timeout=180s")
                scheduled = self.kubectl("get", "pod/" + pod, "-o", "jsonpath={.spec.nodeName}")
                require(scheduled == host["name"], "GPU selector did not schedule onto the intended worker")
            report.append({"name": host["name"], "bootstrap_labels": labels, "disable_ipv6": ipv6,
                           "management": host["management"], "ovs_bridge": False, "schedulable": True})
        (self.state / "host-provisioning-validation.json").write_text(json.dumps(report, indent=2))
        print("PASS DPU-host kubelet labels, no host OVS bridge, IPv6 and schedulability", flush=True)

    def verify_dpu_cni(self):
        cluster = self.nke("GET", "/clusters/" + self.config["cluster_ids"]["dpu"])
        require(cluster["cniPlugin"] == "ovn-kubernetes", "The DPU cluster must be provisioned with OVN as its primary CNI")
        daemons = json.loads(self.dpu_kubectl("get", "daemonsets", "-A", "-o", "json"))["items"]
        require(not any(any(cni in d["metadata"]["name"] for cni in ["calico", "flannel", "kube-proxy"]) for d in daemons),
                "A previous primary CNI or kube-proxy remains in the DPU cluster")
        report = {"cluster": cluster["name"], "cniPlugin": cluster["cniPlugin"], "nodes": []}
        for node in self.dpus:
            primary = self.exec(node["container"], "cat", "/etc/cni/net.d/10-ovn-kubernetes.conf")
            require(json.loads(primary)["type"] == "ovn-k8s-cni-overlay", "The DPU kubelet does not delegate to OVN")
            root_ports = self.exec(node["container"], "ovs-vsctl", "list-ports", "br-int").splitlines()
            offload_ports = self.dpu_exec(node["container"], "ovs-vsctl", "list-ports", "br-int").splitlines()
            require(not any(p.startswith("rep0-") for p in root_ports), "Primary OVN owns a secondary VF representor")
            require(any(p.startswith("rep0-") for p in offload_ports), "The VMaaS instance has no secondary pod representor")
            script = """import json,os,socket
from pathlib import Path
s=socket.socket(socket.AF_INET6,socket.SOCK_STREAM);s.bind(('::1',0));s.close()
print(json.dumps({'netns_inode':os.stat('/proc/self/ns/net').st_ino,
    'disable_ipv6':{n:(Path('/proc/sys/net/ipv6/conf')/n/'disable_ipv6').read_text().strip() for n in ['all','default','lo']}}))"""
            root = json.loads(self.exec(node["container"], "python3", "-c", script))
            offload = json.loads(self.dpu_exec(node["container"], "python3", "-c", script))
            require(root["netns_inode"] != offload["netns_inode"], "Primary and offload OVN share a network namespace")
            require(all(v == "0" for state in [root, offload] for v in state["disable_ipv6"].values()), "IPv6 is disabled for FRR's sockets")
            paths = ["/var/run/openvswitch/db.sock", OFFLOAD_RUN_DIR + "/db.sock"]
            inodes = json.loads(self.exec(node["container"], "python3", "-c",
                                         "import json,os,sys; print(json.dumps([os.stat(p).st_ino for p in sys.argv[1:]]))", *paths))
            require(inodes[0] != inodes[1], "Primary and offload OVN share an OVS database socket")
            report["nodes"].append({"name": node["name"], "primary": root, "offload": offload,
                                    "ovs_socket_paths": paths, "primary_ports": root_ports, "offload_ports": offload_ports})
        (self.state / "dpu-primary-validation.json").write_text(json.dumps(report, indent=2))
        print("PASS DPU primary CNI is OVN; primary/offload namespaces, OVS databases and representors are isolated; IPv6 sockets work", flush=True)

    def mark_validated(self):
        self.config["validated"] = True
        self.save()
        latest = STATE / "managed-latest"
        latest.unlink(missing_ok=True)
        latest.symlink_to(self.state.name, target_is_directory=True)

    def bootstrap_frr_webhook(self):
        # FRR configuration must exist before OVN creates the advertised
        # default network. Reach its real host-network admission server directly
        # until ClusterIP forwarding exists; retain TLS verification and Fail.
        cert = self.state / "frr-webhook.crt"
        key = self.state / "frr-webhook.key"
        ip = self.hosts[0]["ip"]
        command("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", str(key),
                "-out", str(cert), "-days", "1", "-subj", "/CN=nke-dpu-lab-frr-webhook",
                "-addext", "subjectAltName=IP:" + ip + ",DNS:frr-k8s-webhook-service.frr-k8s-system.svc")
        key.chmod(0o600)
        secret = json.loads(self.kubectl("get", "secret/frr-k8s-webhook-server-cert", "-n", "frr-k8s-system", "-o", "json"))
        ca = base64.b64encode(cert.read_bytes()).decode()
        secret["data"] = {"tls.crt": ca, "ca.crt": ca, "tls.key": base64.b64encode(key.read_bytes()).decode()}
        secret["metadata"].pop("managedFields", None)
        self.kubectl("replace", "-f", "-", input=yaml.safe_dump(secret))
        deployment = json.loads(self.kubectl("get", "deployment/frr-k8s-statuscleaner", "-n", "frr-k8s-system", "-o", "json"))
        deployment["spec"]["template"]["spec"]["nodeSelector"] = {"kubernetes.io/hostname": self.hosts[0]["name"]}
        container = deployment["spec"]["template"]["spec"]["containers"][0]
        container["args"] = [a for a in container.get("args", []) if not a.startswith("--disable-cert-rotation")]
        container["args"].append("--disable-cert-rotation")
        deployment["spec"]["strategy"] = {"type": "Recreate"}
        deployment["spec"]["template"]["metadata"].setdefault("annotations", {})["nke-dpu-lab/webhook-cert-sha256"] = hashlib.sha256(cert.read_bytes()).hexdigest()
        self.kubectl("replace", "-f", "-", input=yaml.safe_dump(deployment))
        webhook = json.loads(self.kubectl("get", "validatingwebhookconfiguration/frr-k8s-validating-webhook-configuration", "-o", "json"))
        for entry in webhook["webhooks"]:
            config = entry["clientConfig"]
            service = config.pop("service", None)
            path = service["path"] if service else urllib.parse.urlparse(config["url"]).path
            config["url"] = f"https://{ip}:19443" + path
            config["caBundle"] = ca
            require(entry["failurePolicy"] == "Fail", "FRR admission must stay enabled")
        self.kubectl("replace", "-f", "-", input=yaml.safe_dump(webhook))
        print("FRR bootstrap admission uses its host-network TLS endpoint; validation remains enabled", flush=True)

    def verify_management(self):
        clusters = {role: self.nke("GET", "/clusters/" + cid) for role, cid in self.config["cluster_ids"].items()}
        require(all(self.ready(role) for role in clusters), "NKE has not marked both clusters provisioned")
        require(self.nodes_ready("dpu") and self.nodes_ready("vmaas"), "Expected all requested nodes Ready in each tenant cluster")
        expected = {"provision-cluster-" + cid for cid in self.config["cluster_ids"].values()}
        expected.update("provision-node-" + n["node_id"] for n in self.dpus + self.hosts)
        def completed_workflows():
            runs = json.loads(self.mgmt("exec", "-n", "cloud-workflow-engine", "deployment/temporal", "--", "temporal",
                                       "workflow", "list", "--address", "temporal:7233", "--output", "json"))
            selected = {r["execution"]["workflowId"]: r for r in runs if r["execution"]["workflowId"] in expected}
            return selected if len(selected) == len(expected) and all(r["status"] == "WORKFLOW_EXECUTION_STATUS_COMPLETED"
                                                                      for r in selected.values()) else None
        workflows = wait_until("both cluster and all node provisioning workflows completed", completed_workflows, 180)
        report = {"management_cluster": self.config["management_cluster"], "docker_context": self.config["docker_context"],
                  "clusters": {role: {"id": c["id"], "name": c["name"], "cniPlugin": c["cniPlugin"],
                                      "cniVersion": c["cniVersion"],
                                      "conditions": c.get("conditions")} for role, c in clusters.items()},
                  "dpu_nodes": [{k: n[k] for k in ["name", "node_id", "id", "ip"]} for n in self.dpus],
                  "vmaas_nodes": [{k: n[k] for k in ["name", "node_id", "id", "ip", "dpu_node_name"]} for n in self.hosts],
                  "workflows": [{"id": key, "run_id": r["execution"]["runId"], "status": r["status"]} for key, r in workflows.items()]}
        (self.state / "management-validation.json").write_text(json.dumps(report, indent=2))
        print("PASS NKE management API reports both clusters provisioned; NICo instances and host/DPU pairing recorded", flush=True)

    def cleanup(self):
        for role, cid in self.config["cluster_ids"].items():
            self.nke("DELETE", "/clusters/" + cid)
            print(f"Requested NKE teardown of {role}", flush=True)
        owned_ids = set(self.config["cluster_ids"].values())
        wait_until("NKE teardown of managed nodes and cluster records", lambda: not self.nke("GET", "/nodes")["nodes"] and not any(
            c["id"] in owned_ids for c in self.nke("GET", "/clusters")["clusters"]), 900)
        command("kind", "delete", "cluster", "--name", self.config["management_cluster"])
        ids = command("docker", "ps", "-aq", "--filter", "label=nke-dpu-lab.run=" + self.config["run_id"]).split()
        if ids:
            command("docker", "rm", "-f", *ids)
        for net in command("docker", "network", "ls", "-q", "--filter", "label=nke-dpu-lab.run=" + self.config["run_id"]).split():
            command("docker", "network", "rm", net)
        print("Removed this lab through NKE teardown and run-owned management cleanup", flush=True)


def create(chart, provider_chart, docker_context):
    os.environ["DOCKER_CONTEXT"] = docker_context
    os.environ.pop("DOCKER_HOST", None)
    chart_check(chart)
    # Fail before allocating the lab when a source chart has not been generated.
    command("helm", "template", "capi-provider-nico", str(provider_chart), "--namespace", "capnico-system",
            "--set-string", "manager.image.repository=cluster-api-provider-nico", "--set-string", "manager.image.tag=local")
    for image in [NODE_IMAGE, RUNTIME_IMAGE, KIND_IMAGE, PROVIDER_IMAGE, TEMPORAL_IMAGE, POSTGRES_IMAGE, SIM_IMAGE, PLUGIN_IMAGE]:
        command("docker", "image", "inspect", image)
    run_id = uuid.uuid4().hex[:8]
    name = "nvke-dpu-" + run_id
    state = STATE / ("managed-" + run_id)
    state.mkdir(mode=0o700)
    ids = {str(i): f"{run_id}-0000-0000-0000-{i:012d}" for i in range(1, 6)}
    seed = (ROOT.parents[2] / "controller/nico-sim/internal/seed/default-seed.yaml").read_text()
    seed = seed.replace("00000000-0000-0000-0000-", f"{run_id}-0000-0000-0000-")
    (state / "seed.yaml").write_text(seed)
    command("docker", "network", "create", "--label", "nke-dpu-lab.run=" + run_id, name)
    command("docker", "network", "create", "--label", "nke-dpu-lab.run=" + run_id,
            "--subnet", "172.28.234.0/24", name + "-gateway")
    simulator = name + "-nico-sim"
    command("docker", "create", "--name", simulator, "--network", name, "--label", "nke-dpu-lab.run=" + run_id,
            "-v", "/var/run/docker.sock:/var/run/docker.sock", "-e", "NICO_SIM_LISTEN_ADDR=0.0.0.0:18080",
            "-e", "NICO_SIM_DB_PATH=/state/nico.db", "-e", "NICO_SIM_SEED_PATH=/state/seed.yaml",
            "-e", "NICO_VMS_VM_BACKEND=mock", "-e", "NICO_VMS_BGP_ENABLED=false", "-e", "NICO_VMS_CONSOLE_SSH_PORT=-1",
            "-e", "NICO_CONTAINERS_NODE_NETWORK=" + name, "-e", "NICO_CONTAINERS_NODE_IMAGE=" + NODE_IMAGE,
            "-e", "NICO_CONTAINERS_REQUIRE_HEALTHY=true", "--entrypoint", "nico-sim", RUNTIME_IMAGE)
    command("docker", "cp", str(state / "seed.yaml"), simulator + ":/state/seed.yaml")
    command("docker", "start", simulator)
    wait_until("NICo simulator running", lambda: json.loads(command("docker", "inspect", simulator))[0]["State"]["Running"], 30)
    sim_ip = json.loads(command("docker", "inspect", simulator))[0]["NetworkSettings"]["Networks"][name]["IPAddress"]
    port = free_port()
    config = {"name": name, "run_id": run_id, "ids": ids, "docker_context": docker_context, "hosts": [], "dpu_nodes": [],
              "cluster_ids": {}, "management_cluster": name + "-mgmt", "controller_port": port,
              "controller_api": f"http://127.0.0.1:{port}/api", "nico_endpoint": f"http://{sim_ip}:18080",
              "driver": name + "-driver"}
    (state / "run.json").write_text(json.dumps(config, indent=2))
    command("docker", "run", "-d", "--name", config["driver"], "--label", "nke-dpu-lab.run=" + run_id,
            "--privileged", "--pid=host", "--network=host", "--entrypoint", "/bin/sh", KIND_IMAGE, "-c", "sleep infinity")
    lab = ManagedLab(state)
    print(f"Managed lab state: {state}", flush=True)
    lab.management_up(provider_chart)
    lab.provision_dpu()
    lab.provision_secondary()
    latest = STATE / "managed-latest"
    latest.unlink(missing_ok=True)
    latest.symlink_to(state.name, target_is_directory=True)
    print(f"PASS three-cluster lab. Kubeconfigs and evidence: {state}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["create", "resume", "scale", "verify", "restart", "install", "cleanup"])
    parser.add_argument("--state", type=Path)
    parser.add_argument("--chart", type=Path)
    parser.add_argument("--provider-chart", type=Path)
    parser.add_argument("--docker-context", default="colima")
    parser.add_argument("--workers", type=int, help="Desired workers per cluster for scale")
    args = parser.parse_args()
    if args.action == "create":
        require(args.chart is not None and args.provider_chart is not None, "--chart and --provider-chart are required")
        create(args.chart.resolve(), args.provider_chart.resolve(), args.docker_context)
    else:
        require(args.state is not None, "--state is required")
        lab = ManagedLab(args.state.resolve())
        if args.action == "resume":
            if not lab.config.get("management_ready"):
                require(args.provider_chart is not None, "--provider-chart is required to resume management bootstrap")
                lab.management_up(args.provider_chart.resolve())
            lab.provision_dpu()
            lab.provision_secondary()
        elif args.action == "scale":
            require(args.workers is not None, "--workers is required for scale")
            lab.scale(args.workers)
        elif args.action in ("verify", "restart"):
            lab.verify_management()
            getattr(lab, args.action)()
            lab.mark_validated()
        else:
            getattr(lab, args.action)()


if __name__ == "__main__":
    main()
