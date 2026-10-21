# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Install host and DPU OVN components and supporting CNI resources."""
import base64
import json
import time

import yaml

from .config import STATE, INPUTS, IMAGE, PLUGIN_IMAGE
from .runtime import command, require, wait_until
from .charts import render_ovn
from .dpu_workload_network import isolate_pod, KUBE_NAMESPACE as OFFLOAD_KUBE_NAMESPACE
from . import fabric, validation


def install(sim):
    for name in ["ipamclaims", "multi_network_policy"]:
        sim.kubectl("apply", "--server-side", "-f", "-", input=(STATE / (name + ".yaml")).read_text())
    for host in sim.hosts:
        sim.dpu_exec(host["dpu"], "ovs-vsctl", "set", "Open_vSwitch", ".", "external_ids:ovn-gw-nexthop=172.28.234.254")
    fabric.install(sim)
    # Versioned Multus manifest copied into the ignored cache by setup.
    multus = list(yaml.safe_load_all((STATE / "multus.yaml").read_text()))
    for item in multus:
        if item["kind"] == "ConfigMap":
            cfg = json.loads(item["data"]["daemon-config.json"])
            cfg.update(multusNamespace="default", clusterNetwork="ovn-primary",
                       readinessindicatorfile="/host/etc/cni/net.d/10-ovn-kubernetes.conf")
            item["data"]["daemon-config.json"] = json.dumps(cfg)
        if item["kind"] == "DaemonSet":
            spec = item["spec"]["template"]["spec"]
            spec["initContainers"][0]["command"] = ["sh", "-ec",
                "cp -f /usr/src/multus-cni/bin/multus-shim /host/opt/cni/bin/multus-shim; "
                "cp -f /usr/src/multus-cni/bin/passthru /host/opt/cni/bin/passthru"]
            for c in item["spec"]["template"]["spec"]["containers"]:
                c["resources"] = {"requests": {"cpu": "10m", "memory": "50Mi"}}
                c.setdefault("env", []).extend([{"name": "KUBERNETES_SERVICE_HOST", "value": sim.hosts[0]["ip"]},
                                                 {"name": "KUBERNETES_SERVICE_PORT", "value": "6443"}])
    sim.apply(multus)
    sim.kubectl("wait", "crd/network-attachment-definitions.k8s.cni.cncf.io", "--for=condition=Established", "--timeout=60s")
    sim.apply([{"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition",
                 "metadata": {"name": "ovn-primary", "namespace": "default",
                              "annotations": {"k8s.v1.cni.cncf.io/resourceName": "dpusim.io/vf"}},
                 "spec": {"config": json.dumps({"cniVersion": "1.1.0", "name": "ovn-primary",
                           "netAttachDefName": "default/ovn-primary", "type": "ovn-k8s-cni-overlay"})}}])
    plugin = (STATE / "device-plugin/source/deploy/device-plugin/daemonset.yaml").read_text()
    for key, val in {"DPU_SIM_DP_IMAGE": PLUGIN_IMAGE, "DPU_SIM_MGMT_PORT_VFS_COUNT": "2",
                     "DPU_SIM_UPLINK_VFS_COUNT": "1", "DPU_SIM_NUM_PAIRS": "16"}.items():
        plugin = plugin.replace(key, val)
    sim.kubectl("apply", "-f", "-", input=plugin)
    host_docs = render_ovn("host", {"k8sAPIServer": "https://" + sim.hosts[0]["ip"] + ":6443"})
    crds = [d for d in host_docs if d["kind"] == "CustomResourceDefinition"]
    sim.kubectl("apply", "--server-side", "--force-conflicts", "-f", "-", input=yaml.safe_dump_all(crds))
    for crd in crds:
        sim.kubectl("wait", "crd/" + crd["metadata"]["name"], "--for=condition=Established", "--timeout=60s")
    for _ in range(120):
        try:
            sim.kubectl("get", "serviceaccount/ovnkube-node", "-n", "ovn-kubernetes")
            break
        except RuntimeError:
            time.sleep(2)
    else:
        raise RuntimeError("NVKE node-agent has not installed the DPU-host CNI")
    token = sim.service_account_token("ovnkube-node", "ovn-kubernetes")
    ca = sim.exec(sim.hosts[0]["container"], "cat", "/etc/kubernetes/pki/ca.crt")
    docs = render_ovn("dpu", {"global": {
        "dpuHostClusterK8sAPIServer": "https://" + sim.hosts[0]["ip"] + ":6443",
        "dpuHostClusterK8sCACertData": base64.b64encode(ca.encode()).decode()}})
    # Upstream's chart fixes the Kubernetes namespace. The fixture gives its
    # offload instance separate resources and leaves primary OVN's RBAC intact.
    docs = [d for d in docs if d["kind"] not in ("ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition")]
    for doc in docs:
        metadata = doc["metadata"]
        if doc["kind"] == "Namespace":
            metadata["name"] = OFFLOAD_KUBE_NAMESPACE
        elif metadata.get("namespace") == "ovn-kubernetes":
            metadata["namespace"] = OFFLOAD_KUBE_NAMESPACE
        for subject in doc.get("subjects", []):
            if subject.get("namespace") == "ovn-kubernetes":
                subject["namespace"] = OFFLOAD_KUBE_NAMESPACE
        if doc["kind"] == "DaemonSet":
            spec = doc["spec"]["template"]["spec"]
            spec["nodeSelector"] = {"kubernetes.io/os": "linux", "nvke-nais-dpu-sim-offload": ""}
            spec["terminationGracePeriodSeconds"] = 5
            for container in spec["containers"]:
                container.setdefault("env", []).append({"name": "METRICS_IP", "value": "0.0.0.0"})
                for entry in container.get("env", []):
                    if entry["name"] == "OVN_KUBERNETES_NAMESPACE":
                        entry.clear()
                        entry.update(name="OVN_KUBERNETES_NAMESPACE", value="ovn-kubernetes")
                    elif entry["name"] == "K8S_TOKEN":
                        entry.clear()
                        entry.update(name="K8S_TOKEN", valueFrom={
                            "secretKeyRef": {"name": "ovn-host-access", "key": "token"}})
            require(any(e.get("valueFrom", {}).get("secretKeyRef", {}).get("name") == "ovn-host-access"
                        for c in spec["containers"] for e in c.get("env", [])), "Upstream DPU API token must use its Secret")
            isolate_pod(spec)
    access = {"apiVersion": "v1", "kind": "Secret",
              "metadata": {"name": "ovn-host-access", "namespace": OFFLOAD_KUBE_NAMESPACE},
              "stringData": {"token": token}}
    sim.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump_all([d for d in docs if d["kind"] == "Namespace"] + [access]))
    activate_offload(sim)
    sim.dpu_kubectl("apply", "-f", "-", input=yaml.safe_dump_all(docs))
    sim.dpu_kubectl("rollout", "restart", "daemonset/ovnkube-node-dpu", "-n", OFFLOAD_KUBE_NAMESPACE)
    configure_dns(sim)
    sim.kubectl("rollout", "restart", "daemonset/kube-multus-ds", "-n", "kube-system")
    print("Upstream host and DPU profiles installed", flush=True)


def configure_dns(sim):
    dns = {"spec": {"template": {"spec": {"containers": [{"name": "coredns", "resources": {
        "requests": {"dpusim.io/vf": "1"}, "limits": {"dpusim.io/vf": "1"}}}]}}}}
    sim.kubectl("patch", "deployment/coredns", "-n", "kube-system", "--type=strategic", "-p", json.dumps(dns))


def activate_offload(sim):
    registered = {node["metadata"]["name"] for node in json.loads(sim.kubectl("get", "nodes", "-o", "json"))["items"]}
    for host in sim.hosts:
        if host["name"] in registered and not host.get("offload_active"):
            sim.dpu_kubectl("label", "node", host["dpu_node_name"], "nvke-nais-dpu-sim-offload=", "--overwrite")
            host["offload_active"] = True
            sim.save()


def restart(sim):
    sim.kubectl("rollout", "restart", "daemonset/ovnkube-node-dpu-host", "-n", "ovn-kubernetes")
    sim.kubectl("rollout", "status", "daemonset/ovnkube-node-dpu-host", "-n", "ovn-kubernetes", "--timeout=300s")
    sim.dpu_kubectl("rollout", "restart", "daemonset/ovnkube-node", "-n", "ovn-kubernetes")
    sim.dpu_kubectl("rollout", "status", "daemonset/ovnkube-node", "-n", "ovn-kubernetes", "--timeout=300s")
    sim.dpu_kubectl("rollout", "restart", "daemonset/ovnkube-node-dpu", "-n", OFFLOAD_KUBE_NAMESPACE)
    sim.dpu_kubectl("rollout", "status", "daemonset/ovnkube-node-dpu", "-n", OFFLOAD_KUBE_NAMESPACE, "--timeout=300s")
    validation.verify(sim)


def configure_dns_devices(sim):
    # NVKE's DNS cache is a pod-network DaemonSet behind kube-dns. Both it
    # and CoreDNS need a VF from the simulation device plugin.
    for kind, name in [("daemonset", "node-local-dns"), ("deployment", "coredns")]:
        try:
            obj = json.loads(sim.kubectl("get", kind + "/" + name, "-n", "kube-system", "-o", "json"))
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
            sim.kubectl("patch", kind + "/" + name, "-n", "kube-system", "--type=strategic", "-p", json.dumps(patch))


def verify_preloaded_images(sim):
    # Managed node images preload these before the node agent starts.
    for node in [h["container"] for h in sim.hosts] + [h["dpu"] for h in sim.hosts]:
        wait_until("managed node image preload", lambda: sim.exec(node, "systemctl", "is-active", "nvke-nais-dpu-sim-preload.service") == "active", 600)
        for ref in [IMAGE, PLUGIN_IMAGE]:
            expected = command("docker", "image", "inspect", ref, "--format", "{{.Id}}")
            actual = json.loads(sim.exec(node, "crictl", "inspecti", ref))["status"]["id"]
            require(actual == expected, "Managed node preload differs for " + ref)
        multus = INPUTS["multus"]["image"] + "@" + INPUTS["multus"]["digest"]
        installed = json.loads(sim.exec(node, "crictl", "inspecti", multus))
        require(installed["status"]["id"] == INPUTS["multus"]["config_digest"], "Managed node has an unexpected Multus image")
    print("PASS simulation images preloaded by the managed node image", flush=True)
