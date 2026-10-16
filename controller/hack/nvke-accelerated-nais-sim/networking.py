#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Networking fixtures and traffic assertions for the NKE-managed DPU lab."""
import base64
import copy
import hashlib
import json
import subprocess
import sys
import time

import yaml

from lab import ROOT, STATE, IMAGE, command, require
from offload import exec_offload, isolate_pod, NAMESPACE as OFFLOAD_NAMESPACE, KUBE_NAMESPACE as OFFLOAD_KUBE_NAMESPACE

SIM_IMAGE = "nke-vmaas-ovn-sim:a8b97898"
PLUGIN_IMAGE = "nke-dpu-device-plugin:a99c4111"
MULTUS_IMAGE = "ghcr.io/k8snetworkplumbingwg/multus-cni@sha256:452e64da925fbe2fc78749b20a67659d840780ca8e1ad21cfdb63672542f10c2"
FRR_CONTROLLER = "nke-dpu-frr-k8s:b43efcb2"
FRR_IMAGE = "quay.io/frrouting/frr@sha256:eae3340ca578bb588d92ab3d1e77deb806f80d1da2bc0053cb1dfcfef2aa236f"
HTTP_SERVER = """import http.server,json,socket
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  self.send_response(200); self.end_headers()
  self.wfile.write(json.dumps({'hostname':socket.gethostname(),'client':self.client_address[0]}).encode())
http.server.HTTPServer(('0.0.0.0',8080),Handler).serve_forever()
"""


class NetworkingFixtures:
    def __init__(self, state):
        self.state = state
        self.config = json.loads((state / "run.json").read_text())
        self.name = self.config["name"]
        self.hosts = self.config["hosts"]

    def save(self):
        (self.state / "run.json").write_text(json.dumps(self.config, indent=2))

    def management(self, node):
        addresses = json.loads(self.exec(node, "ip", "-j", "-4", "address", "show", "eth0"))
        routes = json.loads(self.exec(node, "ip", "-j", "-4", "route", "show", "default"))
        return {"addresses": sorted([a["local"], a["prefixlen"]] for a in addresses[0]["addr_info"]),
                "default": [{k: r.get(k) for k in ("dst", "gateway", "dev")} for r in routes]}

    def interface(self, node, address):
        links = json.loads(self.exec(node, "ip", "-j", "-4", "address", "show"))
        return next(link["ifname"] for link in links if any(a["local"] == address for a in link.get("addr_info", [])))

    def exec(self, node, *args, **kwargs):
        return command("docker", "exec", "-i", node, *args, **kwargs)

    def dpu_exec(self, node, *args, **kwargs):
        return exec_offload(self, node, *args, **kwargs)

    def kubectl(self, *args, **kwargs):
        for attempt in range(3):
            try:
                return self.exec(self.hosts[0]["container"], "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", *args, **kwargs)
            except RuntimeError as err:
                # Shared Docker VM pressure can delay the API TLS handshake.
                # Retry transport failures only; assertion and API errors still fail.
                if "TLS handshake timeout" not in str(err) or attempt == 2:
                    raise
                print("Retrying workload API after TLS handshake timeout", flush=True)
                time.sleep(2)

    def apply(self, objects):
        self.kubectl("apply", "-f", "-", input=yaml.safe_dump_all(objects))

    def install_frr(self):
        docs = list(yaml.safe_load_all((STATE / "frr-k8s/config/all-in-one/frr-k8s.yaml").read_text()))
        daemon = next(d for d in docs if d["kind"] == "DaemonSet")
        for doc in docs:
            if doc["kind"] not in ("DaemonSet", "Deployment"):
                continue
            spec = doc["spec"]["template"]["spec"]
            spec["tolerations"] = [{"operator": "Exists"}]
            for c in spec.get("initContainers", []) + spec["containers"]:
                c["image"] = FRR_CONTROLLER if "metallb" in c["image"] else FRR_IMAGE
        host_docs = copy.deepcopy([d for d in docs if d["kind"] != "DaemonSet"])
        # Managed bootstrap owns the live webhook endpoint and certificate.
        # Reapplying their initial manifests would overwrite that configuration.
        for doc in host_docs[:]:
            if doc["kind"] not in ("Secret", "ValidatingWebhookConfiguration"):
                continue
            args = ["get", doc["kind"] + "/" + doc["metadata"]["name"], "--ignore-not-found", "-o", "name"]
            if namespace := doc["metadata"].get("namespace"):
                args.extend(["-n", namespace])
            if self.kubectl(*args):
                host_docs.remove(doc)
        for doc in host_docs:
            if doc["kind"] == "Deployment":
                doc["spec"]["strategy"] = {"type": "Recreate"}
                doc["spec"]["template"]["spec"]["nodeSelector"] = {"kubernetes.io/hostname": self.hosts[0]["name"]}
                doc["spec"]["template"]["spec"]["containers"][0].setdefault("args", []).append("--disable-cert-rotation")
                for c in doc["spec"]["template"]["spec"]["containers"]:
                    c["env"].extend([{"name": "KUBERNETES_SERVICE_HOST", "value": self.hosts[0]["ip"]},
                                     {"name": "KUBERNETES_SERVICE_PORT", "value": "6443"}])
        self.apply(host_docs)
        self.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump_all([d for d in docs if d["kind"] != "DaemonSet"]))
        token = self.kubectl("create", "token", "frr-k8s-daemon", "-n", "frr-k8s-system", "--duration=8h")
        ca = self.exec(self.hosts[0]["container"], "cat", "/etc/kubernetes/pki/ca.crt")
        kubeconfig = {"apiVersion": "v1", "kind": "Config", "current-context": "host",
                      "clusters": [{"name": "host", "cluster": {"server": "https://" + self.hosts[0]["ip"] + ":6443",
                                  "certificate-authority-data": base64.b64encode(ca.encode()).decode()}}],
                      "users": [{"name": "host", "user": {"token": token}}],
                      "contexts": [{"name": "host", "context": {"cluster": "host", "user": "host"}}]}
        secret = {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "frr-host-kubeconfig", "namespace": "frr-k8s-system"},
                  "stringData": {"kubeconfig": yaml.safe_dump(kubeconfig)}}
        self.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump(secret))
        for pair, host in enumerate(self.hosts):
            remote = copy.deepcopy(daemon)
            remote["metadata"]["name"] += "-" + str(pair)
            spec = remote["spec"]["template"]["spec"]
            remote["spec"]["selector"]["matchLabels"]["nke-dpu-pair"] = str(pair)
            remote["spec"]["template"]["metadata"]["labels"]["nke-dpu-pair"] = str(pair)
            spec["nodeSelector"] = {"kubernetes.io/hostname": host["dpu"]}
            spec["volumes"].append({"name": "host-kubeconfig", "secret": {"secretName": "frr-host-kubeconfig"}})
            dpu = next(n for n in self.dpus if n["name"] == host["dpu"])
            for c in spec["containers"]:
                if c["name"] == "controller":
                    c["args"] = ["--node-name=" + host["name"] if a.startswith("--node-name=") else a for a in c["args"]]
                    c["env"].append({"name": "KUBECONFIG", "value": "/host-kubeconfig/kubeconfig"})
                    c["volumeMounts"].append({"name": "host-kubeconfig", "mountPath": "/host-kubeconfig", "readOnly": True})
                    c["args"].append("--health-probe-bind-address=" + dpu["offload_ip"] + ":7572")
            isolate_pod(spec, dpu["offload_ip"])
            self.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump(remote))
        print("FRR-K8s watches host-cluster configuration and runs on paired DPUs", flush=True)

    def configure_dns(self):
        dns = {"spec": {"template": {"spec": {"containers": [{"name": "coredns", "resources": {
            "requests": {"dpusim.io/vf": "1"}, "limits": {"dpusim.io/vf": "1"}}}]}}}}
        self.kubectl("patch", "deployment/coredns", "-n", "kube-system", "--type=strategic", "-p", json.dumps(dns))

    def wire(self):
        driver = self.config["driver"]
        for pair, host in enumerate(self.hosts):
            if host.get("wired"):
                continue
            host_pid = json.loads(command("docker", "inspect", host["container"]))[0]["State"]["Pid"]
            dpu_pid = json.loads(command("docker", "inspect", host["dpu"]))[0]["State"]["Pid"]
            self.exec(driver, "nsenter", "-t", str(host_pid), "-n", "sh", "-ec",
                      'for i in $(seq 0 15); do if ! ip link show "eth0-$i" >/dev/null 2>&1; then '
                      'ip link add "eth0-$i" type veth peer name "rep0-$i"; ip link set "rep0-$i" netns "$1"; fi; '
                      'ip link set "eth0-$i" up; done', "wire", str(dpu_pid))
            self.exec(host["dpu"], "sh", "-ec", 'for i in $(seq 0 15); do if ip link show "rep0-$i" >/dev/null 2>&1; then '
                      'ip link set "rep0-$i" netns "$1"; fi; done', "offload", OFFLOAD_NAMESPACE)
            self.dpu_exec(host["dpu"], "sh", "-ec", 'for i in $(seq 0 15); do ip link set "rep0-$i" up; done')
            gateway = f"172.28.234.{20 + pair}"
            self.exec(host["container"], "ip", "addr", "replace", gateway + "/24", "dev", "eth0-0")
            network = self.name + "-gateway"
            details = json.loads(command("docker", "inspect", host["dpu"]))[0]
            if network not in details["NetworkSettings"]["Networks"]:
                command("docker", "network", "connect", network, host["dpu"])
            address = json.loads(command("docker", "inspect", host["dpu"]))[0]["NetworkSettings"]["Networks"][network]["IPAddress"]
            self.exec(host["dpu"], "sh", "-ec", 'if ip link show eth1 >/dev/null 2>&1; then ip link set eth1 netns "$1"; fi',
                      "offload", OFFLOAD_NAMESPACE)
            self.dpu_exec(host["dpu"], "sh", "-ec",
                      'ovs-vsctl --may-exist add-br breth1 -- set Bridge breth1 datapath_type=system; '
                      'ovs-vsctl --may-exist add-port breth1 eth1 -- --may-exist add-port breth1 rep0-0; '
                      'ip addr flush dev eth1; ip link set eth1 up; ip link set breth1 up; '
                      'ip addr replace "$1/24" dev breth1', "gateway", address)
            dpu = next(n for n in self.dpus if n["container"] == host["dpu"])
            self.dpu_exec(host["dpu"], "ovs-vsctl", "set", "Open_vSwitch", ".",
                      "external_ids:host-k8s-nodename=" + host["name"],
                      "external_ids:hostname=" + self.exec(host["dpu"], "hostname"),
                      "external_ids:ovn-encap-ip=" + dpu["offload_ip"],
                      "external_ids:ovn-encap-type=geneve", "external_ids:ovn-gw-interface=eth1",
                      "external_ids:ovn-gw-router-subnet=172.28.234.0/24", "external_ids:ovn-gw-nexthop=172.28.234.254",
                      "external_ids:ovn-bridge-datapath-type=system")
            print(f"Paired {host['name']} <-> {host['dpu']}, 16 VF links", flush=True)
            host["wired"] = True
            self.save()

    def simulation_links(self):
        network = self.name + "-uplink"
        if not command("docker", "network", "ls", "-q", "--filter", "name=^" + network + "$"):
            command("docker", "network", "create", "--label", "nke-dpu-lab.run=" + self.config["run_id"],
                    "--subnet", "172.28.236.0/24", network)
        for pair, host in enumerate(self.hosts):
            if host.get("simulation_links_ready"):
                continue
            for node, role in ((host["container"], "host"), (host["dpu"], "dpu")):
                execute = self.exec if role == "host" else self.dpu_exec
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
            self.exec(host["container"], "ip", "address", "replace", f"172.28.236.{20 + pair}/24", "dev", "eth0-3")
            details = json.loads(command("docker", "inspect", host["dpu"]))[0]
            if network not in details["NetworkSettings"]["Networks"]:
                command("docker", "network", "connect", network, host["dpu"])
                self.exec(host["dpu"], "ip", "link", "set", "eth2", "netns", OFFLOAD_NAMESPACE)
            details = json.loads(command("docker", "inspect", host["dpu"]))[0]
            address = details["NetworkSettings"]["Networks"][network]["IPAddress"]
            self.dpu_exec(host["dpu"], "sh", "-ec",
                      "ovs-vsctl --may-exist add-br breth-uplink; "
                      "ovs-vsctl --may-exist add-port breth-uplink eth2 -- --may-exist add-port breth-uplink rep0-3; "
                      "ovs-vsctl br-set-external-id breth-uplink bridge-uplink eth2; "
                      "ip address flush dev eth2; ip link set eth2 up; ip link set breth-uplink up; "
                      'ip address replace "$1/24" dev breth-uplink', "uplink", address)
            host["simulation_links_ready"] = True
            self.save()
        print("Configured deterministic simulated MACs and reserved host uplinks", flush=True)

    def load_helpers(self):
        for host in self.hosts:
            node = host["container"]
            command("docker", "cp", str(STATE / "multus-oci.tar"), node + ":/var/lib/multus-oci.tar")
            self.exec(node, "ctr", "-n", "k8s.io", "images", "import", "--no-unpack", "--digests",
                      "--base-name", "ghcr.io/k8snetworkplumbingwg/multus-cni", "/var/lib/multus-oci.tar")
            # OCI archives add an outer index. Register the preserved ARM
            # manifest under its own registry digest, rather than that wrapper.
            self.exec(node, "ctr", "-n", "k8s.io", "images", "tag", "--force",
                      "ghcr.io/k8snetworkplumbingwg/multus-cni:multus", MULTUS_IMAGE)
            self.exec(node, "rm", "/var/lib/multus-oci.tar")
            image = json.loads(self.exec(node, "crictl", "inspecti", MULTUS_IMAGE))
            require(image["status"]["id"] == "sha256:34432c1198661bd95ef909fb1337ffda84ed4c8585090c6da8f2d59013fbaa45",
                    "Unexpected Multus image identity")
        print("Pinned Multus OCI image preloaded on all compute hosts", flush=True)

    @staticmethod
    def simulation_image(docs):
        for doc in docs:
            if doc and doc["kind"] in ("DaemonSet", "Deployment"):
                spec = doc["spec"]["template"]["spec"]
                for c in spec.get("initContainers", []) + spec["containers"]:
                    if c["image"] == IMAGE:
                        c["image"] = SIM_IMAGE
                        c["imagePullPolicy"] = "IfNotPresent"
                    for env in c.get("env", []):
                        if env["name"] == "OVNKUBE_LOGLEVEL":
                            env["value"] = "2"

    def install(self):
        for host in self.hosts:
            self.dpu_exec(host["dpu"], "ovs-vsctl", "set", "Open_vSwitch", ".", "external_ids:ovn-gw-nexthop=172.28.234.254")
        self.install_frr()
        # Versioned Multus manifest copied into the ignored cache by setup.
        multus = list(yaml.safe_load_all((STATE / "multus.yaml").read_text()))
        for item in multus:
            if item["kind"] == "ConfigMap":
                cfg = json.loads(item["data"]["daemon-config.json"])
                cfg.update(multusNamespace="default", clusterNetwork="cluster-network",
                           readinessindicatorfile="/host/etc/cni/net.d/10-ovn-kubernetes.conf")
                item["data"]["daemon-config.json"] = json.dumps(cfg)
            if item["kind"] == "DaemonSet":
                for c in item["spec"]["template"]["spec"]["containers"]:
                    c["resources"] = {"requests": {"cpu": "10m", "memory": "50Mi"}}
                    c.setdefault("env", []).extend([{"name": "KUBERNETES_SERVICE_HOST", "value": self.hosts[0]["ip"]},
                                                     {"name": "KUBERNETES_SERVICE_PORT", "value": "6443"}])
        self.apply(multus)
        self.kubectl("wait", "crd/network-attachment-definitions.k8s.cni.cncf.io", "--for=condition=Established", "--timeout=60s")
        plugin = (STATE / "device-plugin/source/deploy/device-plugin/daemonset.yaml").read_text()
        for key, val in {"DPU_SIM_DP_IMAGE": PLUGIN_IMAGE, "DPU_SIM_MGMT_PORT_VFS_COUNT": "2",
                         "DPU_SIM_UPLINK_VFS_COUNT": "1", "DPU_SIM_NUM_PAIRS": "16"}.items():
            plugin = plugin.replace(key, val)
        self.kubectl("apply", "-f", "-", input=plugin)
        base = STATE / "chart/ovn-kubernetes"
        common = {"k8sAPIServer": "https://" + self.hosts[0]["ip"] + ":6443",
                  "global": {"imagePullSecretName": "", "skipCallToK8s": True}}
        runtime = self.state / "values-runtime.yaml"
        runtime.write_text(yaml.safe_dump(common))
        host = command("helm", "template", "ovn-kubernetes", str(base), "-n", "ovn-kubernetes",
                       "--include-crds",
                       "-f", str(base / "values-images.yaml"), "-f", str(base / "values-vmaas-nooverlay-dpu-host.yaml"),
                       "-f", str(ROOT / "values-host-sim.yaml"), "-f", str(runtime))
        host_docs = list(yaml.safe_load_all(host))
        crds = [d for d in host_docs if d and d["kind"] == "CustomResourceDefinition"]
        self.kubectl("apply", "--server-side", "--force-conflicts", "-f", "-", input=yaml.safe_dump_all(crds))
        for crd in crds:
            self.kubectl("wait", "crd/" + crd["metadata"]["name"], "--for=condition=Established", "--timeout=60s")
        for _ in range(120):
            try:
                self.kubectl("get", "serviceaccount/ovn-node-dpu-host", "-n", "ovn-kubernetes")
                break
            except RuntimeError:
                time.sleep(2)
        else:
            raise RuntimeError("NKE node-agent has not installed the DPU-host CNI profile")
        self.apply([{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
                     "metadata": {"name": "lab-dpu-ovn-node"},
                     "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "ovn-node"},
                     "subjects": [{"kind": "ServiceAccount", "name": "ovn-node-dpu-host", "namespace": "ovn-kubernetes"}]}])
        # The DPU controller uses a short-lived host-cluster ServiceAccount token.
        token = self.kubectl("create", "token", "ovn-node-dpu-host", "-n", "ovn-kubernetes", "--duration=8h")
        ca = self.exec(self.hosts[0]["container"], "cat", "/etc/kubernetes/pki/ca.crt")
        dpu_values = {**common, "k8sToken": token, "k8sCACertData": base64.b64encode(ca.encode()).decode()}
        runtime.write_text(yaml.safe_dump(dpu_values))
        runtime.chmod(0o600)
        rendered = command("helm", "template", "ovn-kubernetes", str(base), "-n", "ovn-kubernetes",
                           "-f", str(base / "values-images.yaml"), "-f", str(base / "values-vmaas-nooverlay-dpu.yaml"),
                           "-f", str(ROOT / "values-dpu-sim.yaml"), "-f", str(runtime))
        rendered = command(sys.executable, str(ROOT / "simulate-dpu.py"), input=rendered)
        docs = [d for d in yaml.safe_load_all(rendered) if d]
        for doc in docs:
            if doc and doc["kind"] == "DaemonSet":
                doc["spec"]["template"]["spec"]["terminationGracePeriodSeconds"] = 5
                for init in doc["spec"]["template"]["spec"]["initContainers"]:
                    if init["name"] == "ovnkube-init":
                        # Physical init reads BlueField p0 sysfs. The fixture already
                        # supplied the same OVS IDs, so validate that pairing here.
                        init["command"] = ["sh", "-ec", "test -n \"$(ovs-vsctl get Open_vSwitch . external_ids:host-k8s-nodename)\""]
                isolate_pod(doc["spec"]["template"]["spec"])
        self.simulation_image(docs)
        self.dpu_kubectl("label", "nodes", "--all", "k8s.ovn.org/dpu=", "--overwrite")
        nad_crd = next(c for c in crds if c["metadata"]["name"] == "network-attachment-definitions.k8s.cni.cncf.io")
        self.dpu_kubectl("apply", "--server-side", "--force-conflicts", "-f", "-", input=yaml.safe_dump(nad_crd))
        # Secret material is sent on stdin and is never part of tracked fixtures.
        # The offload daemon uses the secondary API and needs no cluster-wide
        # RBAC in the DPU cluster. Preserve primary OVN's roles and bindings.
        docs = [d for d in docs if d["kind"] not in ["ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition"]]
        self.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump_all(docs))
        self.dpu_kubectl("rollout", "restart", "daemonset/ovnkube-node-dpu", "-n", OFFLOAD_KUBE_NAMESPACE)
        runtime.unlink()
        self.configure_dns()
        print("VMaaS host and DPU profiles installed", flush=True)

    def routing(self):
        router = self.name + "-router"
        external = self.name + "-external"
        network = self.name + "-external"
        if not command("docker", "network", "ls", "-q", "--filter", "name=^" + network + "$"):
            command("docker", "network", "create", "--label", "nke-dpu-lab.run=" + self.config["run_id"], "--subnet", "172.28.235.0/24", network)
        if not command("docker", "ps", "-q", "--filter", "name=^/" + router + "$"):
            command("docker", "run", "-d", "--name", router, "--privileged", "--network", self.name + "-gateway",
                    "--ip", "172.28.234.254", "--label", "nke-dpu-lab.run=" + self.config["run_id"], FRR_IMAGE)
            command("docker", "network", "connect", "--ip", "172.28.235.2", network, router)
            self.exec(router, "sh", "-ec", 'sed -i "s/bgpd=no/bgpd=yes/" /etc/frr/daemons')
            command("docker", "restart", router)
        for _ in range(30):
            try:
                self.exec(router, "vtysh", "-c", "show version")
                break
            except RuntimeError:
                time.sleep(1)
        # The fabric reaches host management endpoints through their gateway VFs. A Docker
        # bridge default route would make the management return path asymmetric.
        gateway_iface = self.interface(router, "172.28.234.254")
        for pair, host in enumerate(self.hosts):
            self.exec(router, "ip", "route", "replace", host["ip"] + "/32", "via", f"172.28.234.{20 + pair}", "dev", gateway_iface)
        self.kubectl("rollout", "status", "deployment/frr-k8s-statuscleaner", "-n", "frr-k8s-system", "--timeout=180s")
        for pair, host in enumerate(self.hosts):
            # FRR listens on the DPU's gateway bridge, not the compute host.
            details = json.loads(command("docker", "inspect", host["dpu"]))[0]
            peer_ip = details["NetworkSettings"]["Networks"][self.name + "-gateway"]["IPAddress"]
            self.dpu_exec(host["dpu"], "ip", "address", "replace", peer_ip + "/24", "dev", "breth1")
            self.dpu_exec(host["dpu"], "ip", "link", "set", "breth1", "up")
            self.exec(router, "vtysh", "-c", "configure terminal", "-c", "router bgp 64513",
                      "-c", "bgp router-id 172.28.234.254", "-c", "no bgp ebgp-requires-policy",
                      "-c", f"neighbor {peer_ip} remote-as 64512", "-c", "address-family ipv4 unicast",
                      "-c", f"neighbor {peer_ip} as-override", "-c", f"neighbor {peer_ip} next-hop-self")
            self.dpu_exec(host["dpu"], "ovs-vsctl", "set", "Open_vSwitch", ".", "external_ids:ovn-gw-nexthop=172.28.234.254")
            config = {"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
                      "metadata": {"name": "fabric-" + str(pair), "namespace": "frr-k8s-system",
                                   "labels": {"nais.nvidia.com/frr-config": "default-cdn"}},
                      "spec": {"nodeSelector": {"matchLabels": {"kubernetes.io/hostname": host["name"]}},
                               "bgp": {"routers": [{"asn": 64512, "id": peer_ip, "neighbors": [{
                                   "address": "172.28.234.254", "asn": 64513, "sourceaddress": peer_ip,
                                   "toAdvertise": {"allowed": {"mode": "all"}}, "toReceive": {"allowed": {"mode": "all"}}}]}]}}}
            # Service forwarding becomes available as the DPU-host CNI starts.
            # Keep real admission enabled while waiting for that transport.
            for attempt in range(90):
                try:
                    self.apply([config])
                    break
                except RuntimeError as err:
                    transient = "failed calling webhook" in str(err) and any(
                        detail in str(err) for detail in ["connection refused", "no route to host", "context deadline exceeded", "i/o timeout"])
                    if not transient or attempt == 89:
                        raise
                    time.sleep(2)
        if not command("docker", "ps", "-q", "--filter", "name=^/" + external + "$"):
            command("docker", "run", "-d", "--name", external, "--hostname", external, "--cap-add=NET_ADMIN", "--network", network,
                    "--ip", "172.28.235.3", "--label", "nke-dpu-lab.run=" + self.config["run_id"],
                    "--entrypoint", "python3", IMAGE, "-c", HTTP_SERVER)
            self.exec(external, "ip", "route", "replace", "default", "via", "172.28.235.2")
        print("External FRR router and HTTP endpoint created; RouteAdvertisements selects the fabric configurations", flush=True)

    def verify(self):
        image_id = command("docker", "image", "inspect", SIM_IMAGE, "--format", "{{.Id}}")
        for node in [h["container"] for h in self.hosts] + [h["dpu"] for h in self.hosts]:
            installed = json.loads(self.exec(node, "crictl", "inspecti", SIM_IMAGE))
            require(installed["status"]["id"] == image_id, "Node has a stale simulation image: " + node)
        print("PASS all host and DPU runtimes contain the built NVIDIA simulation image", flush=True)
        for kubectl, daemon, namespace in ((self.kubectl, "ovnkube-node-dpu-host", "ovn-kubernetes"),
                                           (self.dpu_kubectl, "ovnkube-node-dpu", OFFLOAD_KUBE_NAMESPACE),
                                           (self.dpu_kubectl, "ovnkube-node", "ovn-kubernetes")):
            kubectl("rollout", "status", "daemonset/" + daemon, "-n", namespace, "--timeout=300s")
            running = json.loads(kubectl("get", "pods", "-n", namespace, "-o", "json"))["items"]
            images = [c for pod in running for c in pod["status"].get("containerStatuses", [])
                      if c["image"].endswith(SIM_IMAGE)]
            require(len(images) >= 2, "Expected OVN simulation containers on both nodes")
            require(all(c["imageID"] == image_id and "running" in c["state"] for c in images),
                    "OVN must be running the built simulation image")
        print("PASS host and DPU OVN containers run the built simulation image", flush=True)
        self.kubectl("wait", "nodes", "--all", "--for=condition=Ready", "--timeout=300s")
        pods = []
        for pair, host in enumerate(self.hosts):
            pods.append({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "vf-http-" + str(pair), "labels": {"app": "vf-http"}},
                         "spec": {"nodeName": host["name"], "containers": [{"name": "http", "image": IMAGE,
                             "imagePullPolicy": "IfNotPresent", "command": ["python3", "-c", HTTP_SERVER],
                             "resources": {"limits": {"dpusim.io/vf": "1"}},
                             "readinessProbe": {"httpGet": {"path": "/", "port": 8080}, "periodSeconds": 2}}]}})
        self.apply(pods)
        self.kubectl("wait", "pods", "-l", "app=vf-http", "--for=condition=Ready", "--timeout=300s")
        addresses = {p["metadata"]["name"]: p["status"]["podIP"] for p in json.loads(self.kubectl("get", "pods", "-l", "app=vf-http", "-o", "json"))["items"]}
        self.kubectl("wait", "routeadvertisements/advertise-default-network", "--for=condition=Accepted", "--timeout=180s")
        for _ in range(90):
            bgp = json.loads(self.exec(self.name + "-router", "vtysh", "-c", "show bgp ipv4 unicast summary json"))
            peers = bgp.get("ipv4Unicast", bgp).get("peers", {})
            if len(peers) == len(self.hosts) and all(p["state"] == "Established" for p in peers.values()):
                break
            time.sleep(2)
        else:
            raise RuntimeError("All DPU BGP sessions must be Established")
        print("PASS all DPU BGP sessions Established; default-network RouteAdvertisements Accepted", flush=True)
        capture_name = self.name + "-capture"
        capture = subprocess.Popen(["docker", "run", "--rm", "--name", capture_name,
                                    "--label", "nke-dpu-lab.run=" + self.config["run_id"],
                                    "--network", "container:" + self.name + "-router", "--cap-add=NET_RAW",
                                    "--entrypoint", "python3", SIM_IMAGE, "-u", "-c",
                                    (ROOT / "fabric-capture.py").read_text(), addresses["vf-http-0"], addresses["vf-http-1"],
                                    self.interface(self.name + "-router", "172.28.234.254")],
                                   text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            require(capture.stdout.readline().strip() == "READY", "Fabric packet capture did not start")
            for source, target in (("vf-http-0", "vf-http-1"), ("vf-http-1", "vf-http-0")):
                answer = json.loads(self.kubectl("exec", source, "--", "curl", "--noproxy", "*", "-fsS", "--max-time", "10", "http://" + addresses[target] + ":8080"))
                require(answer["hostname"] == target, "Cross-host HTTP reached the wrong pod")
                require(answer["client"] == addresses[source], "Cross-host pod source address was changed")
                print(f"PASS {source} -> {target} over simulated VFs and DPU OVS", flush=True)
            observed, errors = capture.communicate(timeout=35)
            require(capture.returncode == 0 and len(json.loads(observed)) == 2,
                    "Both pod TCP directions must cross the FRR fabric without encapsulation: " + errors)
        finally:
            subprocess.run(["docker", "rm", "-f", capture_name], capture_output=True)
            capture.wait(timeout=10)
        print("PASS fabric capture observed unencapsulated pod TCP in both directions", flush=True)
        for name, address in addresses.items():
            inbound = json.loads(self.exec(self.name + "-external", "curl", "--noproxy", "*", "-fsS", "--max-time", "10", "http://" + address + ":8080"))
            require(inbound["hostname"] == name and inbound["client"] == "172.28.235.3", "External-to-pod HTTP or source preservation failed")
            outbound = json.loads(self.kubectl("exec", name, "--", "curl", "--noproxy", "*", "-fsS", "--max-time", "10", "http://172.28.235.3:8080"))
            require(outbound["hostname"] == self.name + "-external", "Pod-to-external HTTP failed")
            host_index = int(name.rsplit("-", 1)[1])
            require(outbound["client"] == f"172.28.234.{20 + host_index}", "VMaaS outbound SNAT used an unexpected source")
            print(f"PASS external <-> {name}; outbound source {outbound['client']} (VMaaS SNAT enabled)", flush=True)
        self.apply([{"apiVersion": "v1", "kind": "Service", "metadata": {"name": "vf-http"},
                     "spec": {"selector": {"app": "vf-http"}, "ports": [{"port": 8080, "targetPort": 8080}]}}])
        self.kubectl("rollout", "status", "deployment/coredns", "-n", "kube-system", "--timeout=120s")
        print("PASS CoreDNS rollout on the VF primary network", flush=True)
        service_ip = self.kubectl("get", "service/vf-http", "-o", "jsonpath={.spec.clusterIP}")
        check = """import json,socket,ssl,urllib.request
from pathlib import Path
name='vf-http.default.svc.cluster.local'
address=socket.gethostbyname(name)
http=json.load(urllib.request.urlopen('http://'+name+':8080',timeout=10))
root=Path('/var/run/secrets/kubernetes.io/serviceaccount')
context=ssl.create_default_context(cafile=str(root/'ca.crt'))
request=urllib.request.Request('https://kubernetes.default.svc/version',headers={'Authorization':'Bearer '+(root/'token').read_text().strip()})
api=json.load(urllib.request.urlopen(request,context=context,timeout=10))
review=urllib.request.Request('https://kubernetes.default.svc/apis/authentication.k8s.io/v1/selfsubjectreviews',
    data=json.dumps({'apiVersion':'authentication.k8s.io/v1','kind':'SelfSubjectReview'}).encode(),
    headers={'Authorization':request.get_header('Authorization'),'Content-Type':'application/json'})
identity=json.load(urllib.request.urlopen(review,context=context,timeout=10))['status']['userInfo']['username']
print(json.dumps({'address':address,'http':http,'version':api['gitVersion'],'identity':identity}))
"""
        service_checks = {}
        for name in addresses:
            answer=json.loads(self.kubectl("exec", name, "--", "python3", "-c", check))
            require(answer["address"] == service_ip, "DNS did not resolve the HTTP ClusterIP")
            require(answer["http"]["hostname"] in addresses, "Service reached an unexpected endpoint")
            require(answer["version"] == "v1.34.8", "Kubernetes API Service reached the wrong cluster")
            require(answer["identity"] == "system:serviceaccount:default:default", "API did not authenticate the pod's service account")
            service_checks[name] = answer
            print(f"PASS {name}: DNS, HTTP ClusterIP and authenticated Kubernetes API Service", flush=True)
        for host in self.hosts:
            if "management" in host:
                require(self.management(host["container"]) == host["management"],
                        "Host management IPv4 address or default route changed")
        print("PASS host management IPv4 addresses and default routes preserved", flush=True)
        (self.state / "validation.json").write_text(json.dumps({"pods": addresses, "bgp": bgp,
            "base_image": IMAGE, "simulation_image": SIM_IMAGE, "simulation_image_id": image_id,
            "service_checks": service_checks}, indent=2))

    def restart(self):
        self.kubectl("rollout", "restart", "daemonset/ovnkube-node-dpu-host", "-n", "ovn-kubernetes")
        self.kubectl("rollout", "status", "daemonset/ovnkube-node-dpu-host", "-n", "ovn-kubernetes", "--timeout=300s")
        self.dpu_kubectl("rollout", "restart", "daemonset/ovnkube-node", "-n", "ovn-kubernetes")
        self.dpu_kubectl("rollout", "status", "daemonset/ovnkube-node", "-n", "ovn-kubernetes", "--timeout=300s")
        self.dpu_kubectl("rollout", "restart", "daemonset/ovnkube-node-dpu", "-n", OFFLOAD_KUBE_NAMESPACE)
        self.dpu_kubectl("rollout", "status", "daemonset/ovnkube-node-dpu", "-n", OFFLOAD_KUBE_NAMESPACE, "--timeout=300s")
        self.verify()
