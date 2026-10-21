#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Verify explicit pod, node, Service and DNS paths in both managed clusters."""
from contextlib import contextmanager
import json
import subprocess
import time

import yaml

from .config import ROOT, IMAGE, WORKLOAD_ROLE
from .runtime import require
from .workloads import HTTP_SERVER

NODE_PORT = 18089


def verify(sim):
    """Exercise pod, host, Service, API, and external paths without repeating probes."""
    rows = []
    report = {"run_id": sim.config["run_id"], "checks": rows}
    path = sim.state / "network-matrix.json"

    def record(cluster, name, source, target, check):
        row = {"cluster": cluster, "path": name, "source": source, "destination": target}
        try:
            row["observed"] = check()
            row["status"] = "passed"
        except RuntimeError as err:
            row.update(status="failed", error=str(err))
        rows.append(row)
        path.write_text(json.dumps(report, indent=2))
        print(f"{row['status'].upper()} {cluster} {name}: {source} -> {target}", flush=True)

    for role, nodes, kubectl, prefix, vf in [
        (WORKLOAD_ROLE, sim.hosts, sim.kubectl, "vf-http", True),
        ("dpu", sim.dpus, sim.dpu_kubectl, "dpu-http", False),
    ]:
        objects = []
        for index, node in enumerate(nodes + [nodes[0]]):
            name = prefix + ("-local" if index == len(nodes) else "-" + str(index))
            container = {"name": "http", "image": IMAGE, "imagePullPolicy": "IfNotPresent", "command": ["python3", "-c", HTTP_SERVER],
                         "readinessProbe": {"httpGet": {"path": "/", "port": 8080}, "periodSeconds": 2}}
            if vf:
                container["resources"] = {"limits": {"dpusim.io/vf": "1"}}
            objects.append({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": {"app": "nvke-nais-dpu-sim-network-matrix"}},
                            "spec": {"nodeName": node["name"], "containers": [container]}})
        for node in nodes:
            node_server(sim, node)
        kubectl("apply", "-f", "-", input=yaml.safe_dump_all(objects))
        kubectl("wait", "pod", "-l", "app=nvke-nais-dpu-sim-network-matrix", "--for=condition=Ready", "--timeout=180s")
        pod_names = [prefix + "-" + str(i) for i in range(len(nodes))] + [prefix + "-local"]
        pods = {p["metadata"]["name"]: p["status"]["podIP"]
                for p in json.loads(kubectl("get", "pods", "-o", "json"))["items"] if p["metadata"]["name"] in pod_names}
        require(len(pods) == len(pod_names), "Expected a pod on each node and a same-node sibling")

        def pod_client(name):
            return lambda *args: kubectl("exec", name, "--", *args)

        def verify_http(client, address, expected, port=8080, source=None, expected_client=None):
            answer = http(client, address, port, source)
            require(answer["hostname"] == expected, "HTTP reached a different endpoint")
            if expected_client:
                require(answer["client"] == expected_client, "Unexpected source IP")
            return answer

        paths = [(0, len(nodes), "pod-to-pod same node"), (len(nodes), 0, "pod-to-pod same node")]
        paths.extend((i, j, "pod-to-pod cross node") for i in range(len(nodes)) for j in range(len(nodes)) if i != j)
        with observe_fabric(sim, role, pods[pod_names[0]], pods[pod_names[1]], report):
            for src, dst, kind in paths:
                a, b = pod_names[src], pod_names[dst]
                record(role, kind, pods[a], pods[b], lambda a=a, b=b: verify_http(pod_client(a), pods[b], b, expected_client=pods[a]))
        for i in range(len(nodes)):
            for j in range(len(nodes)):
                a, node = pod_names[i], nodes[j]
                scope = "same node" if i == j else "other node"
                record(role, "pod-to-node " + scope, pods[a], node["ip"],
                       lambda a=a, node=node: verify_http(pod_client(a), node["ip"], node["name"], NODE_PORT))
                record(role, "node-to-pod " + scope, node["ip"], pods[a],
                       lambda a=a, node=node: verify_http(lambda *args: sim.exec(node["container"], *args), pods[a], a))
            for j, peer in enumerate(nodes):
                if i == j:
                    continue
                node = nodes[i]
                record(role, "node-to-node management", node["ip"], peer["ip"],
                       lambda node=node, peer=peer: verify_http(lambda *args: sim.exec(node["container"], *args), peer["ip"], peer["name"],
                                                              NODE_PORT, source=node["ip"], expected_client=node["ip"]))
                if vf:
                    source, target = f"172.28.234.{20+i}", f"172.28.234.{20+j}"
                    record(role, "node-to-node gateway VF", source, target,
                           lambda node=node, peer=peer, source=source, target=target: verify_http(
                               lambda *args: sim.exec(node["container"], *args), target, peer["name"], NODE_PORT, source, source))

        if vf:
            for index, name in enumerate(pod_names[:-1]):
                record(role, "external-to-pod", "172.28.235.3", pods[name],
                       lambda name=name: verify_http(
                           lambda *args: sim.exec(sim.name + "-external", *args),
                           pods[name], name, expected_client="172.28.235.3"))
                record(role, "pod-to-external", pods[name], "172.28.235.3",
                       lambda name=name, index=index: verify_http(
                           pod_client(name), "172.28.235.3", sim.name + "-external",
                           expected_client=f"172.28.234.{20 + index}"))

        service = {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "nvke-nais-dpu-sim-network-matrix"},
                   "spec": {"selector": {"app": "nvke-nais-dpu-sim-network-matrix"}, "ports": [{"port": 8080, "targetPort": 8080}]}}
        kubectl("apply", "-f", "-", input=yaml.safe_dump(service))
        kubectl("rollout", "status", "deployment/coredns", "-n", "kube-system", "--timeout=120s")
        time.sleep(2)
        service_ip = kubectl("get", "service/nvke-nais-dpu-sim-network-matrix", "-o", "jsonpath={.spec.clusterIP}")
        for node in nodes:
            def node_service(node=node):
                answer = http(lambda *args: sim.exec(node["container"], *args), service_ip)
                require(answer["hostname"] in pod_names, "Service reached an unexpected endpoint")
                return answer
            record(role, "node-to-Service", node["ip"], service_ip, node_service)
        for a in pod_names[:-1]:
            def pod_services(a=a):
                script = """import json,socket,ssl,urllib.request
from pathlib import Path
name='nvke-nais-dpu-sim-network-matrix.default.svc.cluster.local'
address=socket.gethostbyname(name)
answer=json.load(urllib.request.urlopen('http://'+name+':8080',timeout=5))
root=Path('/var/run/secrets/kubernetes.io/serviceaccount')
req=urllib.request.Request('https://kubernetes.default.svc/version',headers={'Authorization':'Bearer '+(root/'token').read_text().strip()})
context=ssl.create_default_context(cafile=str(root/'ca.crt'))
api=json.load(urllib.request.urlopen(req,context=context,timeout=5))
review=urllib.request.Request('https://kubernetes.default.svc/apis/authentication.k8s.io/v1/selfsubjectreviews',
    data=json.dumps({'apiVersion':'authentication.k8s.io/v1','kind':'SelfSubjectReview'}).encode(),
    headers={'Authorization':req.get_header('Authorization'),'Content-Type':'application/json'})
identity=json.load(urllib.request.urlopen(review,context=context,timeout=5))['status']['userInfo']['username']
print(json.dumps({'dns':address,'http':answer,'version':api['gitVersion'],'identity':identity}))
"""
                answer = json.loads(pod_client(a)("python3", "-c", script))
                require(answer["dns"] == service_ip, "DNS resolved an unexpected Service")
                require(answer["http"]["hostname"] in pod_names, "Service reached a different endpoint")
                require(answer["version"] == "v1.34.8", "Authenticated API reached a different cluster")
                require(answer["identity"] == "system:serviceaccount:default:default", "API did not authenticate the pod's service account")
                return answer
            record(role, "pod DNS, Service and authenticated API", pods[a], service_ip, pod_services)
    report["passed"] = sum(row["status"] == "passed" for row in rows)
    report["failed"] = len(rows) - report["passed"]
    path.write_text(json.dumps(report, indent=2))
    require(report["failed"] == 0, f"Network matrix has {report['failed']} failures; inspect {path}")
    print(f"PASS all {len(rows)} network-matrix checks; evidence: {path}", flush=True)


@contextmanager
def observe_fabric(sim, role, first, second, report):
    """Observe existing cross-host probes on the router instead of sending another set."""
    if role != WORKLOAD_ROLE:
        yield
        return
    sim.kubectl("wait", "routeadvertisements/advertise-default-network", "--for=condition=Accepted", "--timeout=180s")
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        bgp = json.loads(sim.exec(sim.name + "-router", "vtysh", "-c", "show bgp ipv4 unicast summary json"))
        peers = bgp.get("ipv4Unicast", bgp).get("peers", {})
        if len(peers) == len(sim.hosts) and all(peer["state"] == "Established" for peer in peers.values()):
            break
        time.sleep(2)
    else:
        raise RuntimeError("All DPU BGP sessions must be Established")
    capture_name = sim.name + "-capture"
    process = subprocess.Popen([
        "docker", "run", "--rm", "--name", capture_name,
        "--label", "nvke-nais-dpu-sim.run=" + sim.config["run_id"],
        "--network", "container:" + sim.name + "-router", "--cap-add=NET_RAW",
        "--entrypoint", "python3", IMAGE, "-u", "-c",
        (ROOT / "tools/fabric-capture.py").read_text(), first, second,
        sim.interface(sim.name + "-router", "172.28.234.254"),
    ], text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        require(process.stdout.readline().strip() == "READY", "Fabric packet capture did not start")
        yield
        output, errors = process.communicate(timeout=35)
        observed = json.loads(output)
        expected = sorted([[first, second], [second, first]])
        require(process.returncode == 0 and observed == expected,
                "Both pod TCP directions must cross the FRR fabric without encapsulation: " + errors)
        report["fabric"] = {"bgp": bgp, "unencapsulated_tcp_pairs": observed}
        print("PASS fabric capture observed the matrix's cross-host TCP probes in both directions", flush=True)
    finally:
        subprocess.run(["docker", "rm", "-f", capture_name], capture_output=True)
        process.wait(timeout=10)


def node_server(sim, node):
    try:
        answer = json.loads(sim.exec(node["container"], "curl", "--noproxy", "*", "-fsS", "--max-time", "2",
                                     f"http://{node['ip']}:{NODE_PORT}"))
        if answer["hostname"] == node["name"]:
            return
    except RuntimeError:
        pass
    script = HTTP_SERVER.replace("('0.0.0.0',8080)", f"('0.0.0.0',{NODE_PORT})")
    launcher = "import subprocess,sys; p=subprocess.Popen([sys.executable,'-c',sys.argv[1]],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True); print(p.pid)"
    node["matrix_http_pid"] = int(sim.exec(node["container"], "python3", "-c", launcher, script))
    sim.save()


def http(client, address, port=8080, source=None):
    args = ["curl", "--noproxy", "*", "-fsS", "--max-time", "5"]
    if source:
        args += ["--interface", source]
    return json.loads(client(*args, f"http://{address}:{port}"))
