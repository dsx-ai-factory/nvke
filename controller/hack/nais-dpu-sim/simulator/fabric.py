# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Install FRR and connect the routed fabric."""
import base64
import copy
import hashlib
import json
import time
import urllib.error
import urllib.parse

import yaml

from .config import STATE, IMAGE, FRR_CONTROLLER, FRR_IMAGE
from .runtime import command, require
from .workloads import HTTP_SERVER
from .dpu_workload_network import isolate_pod


def install(sim):
    # DaemonSets select Kubernetes node names, not Docker container IDs.
    saved = [(host, host["dpu"]) for host in sim.hosts]
    for host, _ in saved:
        host["dpu"] = host["dpu_node_name"]
    try:
        install_components(sim)
    finally:
        for host, container in saved:
            host["dpu"] = container
    bootstrap_frr_webhook(sim)


def install_components(sim):
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
        if sim.kubectl(*args):
            host_docs.remove(doc)
    for doc in host_docs:
        if doc["kind"] == "Deployment":
            doc["spec"]["strategy"] = {"type": "Recreate"}
            doc["spec"]["template"]["spec"]["nodeSelector"] = {"kubernetes.io/hostname": sim.hosts[0]["name"]}
            doc["spec"]["template"]["spec"]["containers"][0].setdefault("args", []).append("--disable-cert-rotation")
            for c in doc["spec"]["template"]["spec"]["containers"]:
                c["env"].extend([{"name": "KUBERNETES_SERVICE_HOST", "value": sim.hosts[0]["ip"]},
                                 {"name": "KUBERNETES_SERVICE_PORT", "value": "6443"}])
    sim.apply(host_docs)
    sim.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump_all([d for d in docs if d["kind"] != "DaemonSet"]))
    token = sim.service_account_token("frr-k8s-daemon", "frr-k8s-system")
    ca = sim.exec(sim.hosts[0]["container"], "cat", "/etc/kubernetes/pki/ca.crt")
    kubeconfig = {"apiVersion": "v1", "kind": "Config", "current-context": "host",
                  "clusters": [{"name": "host", "cluster": {"server": "https://" + sim.hosts[0]["ip"] + ":6443",
                              "certificate-authority-data": base64.b64encode(ca.encode()).decode()}}],
                  "users": [{"name": "host", "user": {"token": token}}],
                  "contexts": [{"name": "host", "context": {"cluster": "host", "user": "host"}}]}
    secret = {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "frr-host-kubeconfig", "namespace": "frr-k8s-system"},
              "stringData": {"kubeconfig": yaml.safe_dump(kubeconfig)}}
    sim.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump(secret))
    for pair, host in enumerate(sim.hosts):
        remote = copy.deepcopy(daemon)
        remote["metadata"]["name"] += "-" + str(pair)
        remote["spec"]["template"].setdefault("metadata", {}).setdefault("annotations", {})[
            "nvke-nais-dpu-sim/credential-sha256"] = hashlib.sha256(token.encode()).hexdigest()
        spec = remote["spec"]["template"]["spec"]
        remote["spec"]["selector"]["matchLabels"]["nvke-nais-dpu-sim-pair"] = str(pair)
        remote["spec"]["template"]["metadata"]["labels"]["nvke-nais-dpu-sim-pair"] = str(pair)
        spec["nodeSelector"] = {"kubernetes.io/hostname": host["dpu"]}
        spec["volumes"].append({"name": "host-kubeconfig", "secret": {"secretName": "frr-host-kubeconfig"}})
        dpu = next(n for n in sim.dpus if n["name"] == host["dpu"])
        for c in spec["containers"]:
            if c["name"] == "controller":
                c["args"] = ["--node-name=" + host["name"] if a.startswith("--node-name=") else a for a in c["args"]]
                c["env"].append({"name": "KUBECONFIG", "value": "/host-kubeconfig/kubeconfig"})
                c["volumeMounts"].append({"name": "host-kubeconfig", "mountPath": "/host-kubeconfig", "readOnly": True})
                c["args"].append("--health-probe-bind-address=" + dpu["offload_ip"] + ":7572")
        isolate_pod(spec, dpu["offload_ip"])
        sim.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump(remote))
    print("FRR-K8s watches host-cluster configuration and runs on paired DPUs", flush=True)


def bootstrap_frr_webhook(sim):
    # FRR configuration must exist before OVN creates the advertised
    # default network. Reach its real host-network admission server directly
    # until ClusterIP forwarding exists; retain TLS verification and Fail.
    cert = sim.state / "frr-webhook.crt"
    key = sim.state / "frr-webhook.key"
    ip = sim.hosts[0]["ip"]
    command("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", str(key),
            "-out", str(cert), "-days", "365", "-subj", "/CN=nvke-nais-dpu-sim-frr-webhook",
            "-addext", "subjectAltName=IP:" + ip + ",DNS:frr-k8s-webhook-service.frr-k8s-system.svc")
    key.chmod(0o600)
    secret = json.loads(sim.kubectl("get", "secret/frr-k8s-webhook-server-cert", "-n", "frr-k8s-system", "-o", "json"))
    ca = base64.b64encode(cert.read_bytes()).decode()
    secret["data"] = {"tls.crt": ca, "ca.crt": ca, "tls.key": base64.b64encode(key.read_bytes()).decode()}
    secret["metadata"].pop("managedFields", None)
    sim.kubectl("replace", "-f", "-", input=yaml.safe_dump(secret))
    deployment = json.loads(sim.kubectl("get", "deployment/frr-k8s-statuscleaner", "-n", "frr-k8s-system", "-o", "json"))
    deployment["spec"]["template"]["spec"]["nodeSelector"] = {"kubernetes.io/hostname": sim.hosts[0]["name"]}
    container = deployment["spec"]["template"]["spec"]["containers"][0]
    container["args"] = [a for a in container.get("args", []) if not a.startswith("--disable-cert-rotation")]
    container["args"].append("--disable-cert-rotation")
    deployment["spec"]["strategy"] = {"type": "Recreate"}
    deployment["spec"]["template"]["metadata"].setdefault("annotations", {})["nvke-nais-dpu-sim/webhook-cert-sha256"] = hashlib.sha256(cert.read_bytes()).hexdigest()
    sim.kubectl("replace", "-f", "-", input=yaml.safe_dump(deployment))
    webhook = json.loads(sim.kubectl("get", "validatingwebhookconfiguration/frr-k8s-validating-webhook-configuration", "-o", "json"))
    for entry in webhook["webhooks"]:
        config = entry["clientConfig"]
        service = config.pop("service", None)
        path = service["path"] if service else urllib.parse.urlparse(config["url"]).path
        config["url"] = f"https://{ip}:19443" + path
        config["caBundle"] = ca
        require(entry["failurePolicy"] == "Fail", "FRR admission must stay enabled")
    sim.kubectl("replace", "-f", "-", input=yaml.safe_dump(webhook))
    print("FRR bootstrap admission uses its host-network TLS endpoint; validation remains enabled", flush=True)


def routing(sim):
    router = sim.name + "-router"
    external = sim.name + "-external"
    network = sim.name + "-external"
    if not command("docker", "network", "ls", "-q", "--filter", "name=^" + network + "$"):
        command("docker", "network", "create", "--label", "nvke-nais-dpu-sim.run=" + sim.config["run_id"], "--subnet", "172.28.235.0/24", network)
    if not command("docker", "ps", "-q", "--filter", "name=^/" + router + "$"):
        command("docker", "run", "-d", "--name", router, "--privileged", "--network", sim.name + "-gateway",
                "--ip", "172.28.234.254", "--label", "nvke-nais-dpu-sim.run=" + sim.config["run_id"], FRR_IMAGE)
        command("docker", "network", "connect", "--ip", "172.28.235.2", network, router)
        sim.exec(router, "sh", "-ec", 'sed -i "s/bgpd=no/bgpd=yes/" /etc/frr/daemons')
        command("docker", "restart", router)
    for _ in range(30):
        try:
            sim.exec(router, "vtysh", "-c", "show version")
            break
        except RuntimeError:
            time.sleep(1)
    # The fabric reaches host management endpoints through their gateway VFs. A Docker
    # bridge default route would make the management return path asymmetric.
    gateway_iface = sim.interface(router, "172.28.234.254")
    for pair, host in enumerate(sim.hosts):
        sim.exec(router, "ip", "route", "replace", host["ip"] + "/32", "via", f"172.28.234.{20 + pair}", "dev", gateway_iface)
    sim.kubectl("rollout", "status", "deployment/frr-k8s-statuscleaner", "-n", "frr-k8s-system", "--timeout=180s")
    for pair, host in enumerate(sim.hosts):
        # FRR listens on the DPU's gateway bridge, not the compute host.
        details = json.loads(command("docker", "inspect", host["dpu"]))[0]
        peer_ip = details["NetworkSettings"]["Networks"][sim.name + "-gateway"]["IPAddress"]
        sim.dpu_exec(host["dpu"], "ip", "address", "replace", peer_ip + "/24", "dev", "breth1")
        sim.dpu_exec(host["dpu"], "ip", "link", "set", "breth1", "up")
        sim.exec(router, "vtysh", "-c", "configure terminal", "-c", "router bgp 64513",
                  "-c", "bgp router-id 172.28.234.254", "-c", "no bgp ebgp-requires-policy",
                  "-c", f"neighbor {peer_ip} remote-as 64512", "-c", "address-family ipv4 unicast",
                  "-c", f"neighbor {peer_ip} as-override", "-c", f"neighbor {peer_ip} next-hop-self")
        sim.dpu_exec(host["dpu"], "ovs-vsctl", "set", "Open_vSwitch", ".", "external_ids:ovn-gw-nexthop=172.28.234.254")
        config = {"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
                  "metadata": {"name": "fabric-" + str(pair), "namespace": "frr-k8s-system",
                               "labels": {"nvke-nais-dpu-sim": "fabric"}},
                  "spec": {"nodeSelector": {"matchLabels": {"kubernetes.io/hostname": host["name"]}},
                           "bgp": {"routers": [{"asn": 64512, "id": peer_ip, "neighbors": [{
                               "address": "172.28.234.254", "asn": 64513, "sourceaddress": peer_ip,
                               "toAdvertise": {"allowed": {"mode": "all"}}, "toReceive": {"allowed": {"mode": "all"}}}]}]}}}
        # Service forwarding becomes available as the DPU-host CNI starts.
        # Keep real admission enabled while waiting for that transport.
        for attempt in range(90):
            try:
                sim.apply([config])
                break
            except RuntimeError as err:
                transient = "failed calling webhook" in str(err) and any(
                    detail in str(err) for detail in ["connection refused", "no route to host", "context deadline exceeded", "i/o timeout"])
                if not transient or attempt == 89:
                    raise
                time.sleep(2)
    sim.apply([{"apiVersion": "k8s.ovn.org/v1", "kind": "RouteAdvertisements",
                 "metadata": {"name": "advertise-default-network"},
                 "spec": {"networkSelectors": [{"networkSelectionType": "DefaultNetwork"}],
                          "nodeSelector": {}, "frrConfigurationSelector": {"matchLabels": {"nvke-nais-dpu-sim": "fabric"}},
                          "advertisements": ["PodNetwork"]}}])
    if not command("docker", "ps", "-q", "--filter", "name=^/" + external + "$"):
        command("docker", "run", "-d", "--name", external, "--hostname", external, "--cap-add=NET_ADMIN", "--network", network,
                "--ip", "172.28.235.3", "--label", "nvke-nais-dpu-sim.run=" + sim.config["run_id"],
                "--entrypoint", "python3", IMAGE, "-c", HTTP_SERVER)
        sim.exec(external, "ip", "route", "replace", "default", "via", "172.28.235.2")
    print("External FRR router and HTTP endpoint created; RouteAdvertisements selects the fabric configurations", flush=True)
