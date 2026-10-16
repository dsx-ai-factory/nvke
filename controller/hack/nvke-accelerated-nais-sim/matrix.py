#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Verify explicit pod, node, Service and DNS paths in both managed clusters."""
import argparse
import json
from pathlib import Path
import time

import yaml

from lab import IMAGE, require
from networking import HTTP_SERVER

NODE_PORT = 18089


def node_server(lab, node):
    try:
        answer = json.loads(lab.exec(node["container"], "curl", "--noproxy", "*", "-fsS", "--max-time", "2",
                                     f"http://{node['ip']}:{NODE_PORT}"))
        if answer["hostname"] == node["name"]:
            return
    except RuntimeError:
        pass
    script = HTTP_SERVER.replace("('0.0.0.0',8080)", f"('0.0.0.0',{NODE_PORT})")
    launcher = "import subprocess,sys; p=subprocess.Popen([sys.executable,'-c',sys.argv[1]],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True); print(p.pid)"
    node["matrix_http_pid"] = int(lab.exec(node["container"], "python3", "-c", launcher, script))
    lab.save()


def http(client, address, port=8080, source=None):
    args = ["curl", "--noproxy", "*", "-fsS", "--max-time", "5"]
    if source:
        args += ["--interface", source]
    return json.loads(client(*args, f"http://{address}:{port}"))


def verify_networks(lab):
    rows = []
    report = {"run_id": lab.config["run_id"], "checks": rows}
    path = lab.state / "network-matrix.json"

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
        ("vmaas", lab.hosts, lab.kubectl, "vf-http", True),
        ("dpu", lab.dpus, lab.dpu_kubectl, "dpu-http", False),
    ]:
        objects = []
        for index, node in enumerate(nodes + [nodes[0]]):
            name = prefix + ("-local" if index == len(nodes) else "-" + str(index))
            container = {"name": "http", "image": IMAGE, "imagePullPolicy": "IfNotPresent", "command": ["python3", "-c", HTTP_SERVER]}
            if vf:
                container["resources"] = {"limits": {"dpusim.io/vf": "1"}}
            if role == "dpu" or index == len(nodes):
                objects.append({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": {"app": "nke-network-matrix"}},
                                "spec": {"nodeName": node["name"], "containers": [container]}})
            node_server(lab, node)
        kubectl("apply", "-f", "-", input=yaml.safe_dump_all(objects))
        kubectl("wait", "pod", "-l", "app=nke-network-matrix", "--for=condition=Ready", "--timeout=180s")
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
                       lambda a=a, node=node: verify_http(lambda *args: lab.exec(node["container"], *args), pods[a], a))
            for j, peer in enumerate(nodes):
                if i == j:
                    continue
                node = nodes[i]
                record(role, "node-to-node management", node["ip"], peer["ip"],
                       lambda node=node, peer=peer: verify_http(lambda *args: lab.exec(node["container"], *args), peer["ip"], peer["name"],
                                                              NODE_PORT, source=node["ip"], expected_client=node["ip"]))
                if vf:
                    source, target = f"172.28.234.{20+i}", f"172.28.234.{20+j}"
                    record(role, "node-to-node gateway VF", source, target,
                           lambda node=node, peer=peer, source=source, target=target: verify_http(
                               lambda *args: lab.exec(node["container"], *args), target, peer["name"], NODE_PORT, source, source))

        service = {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "nke-network-matrix"},
                   "spec": {"selector": {"app": "nke-network-matrix"}, "ports": [{"port": 8080, "targetPort": 8080}]}}
        kubectl("apply", "-f", "-", input=yaml.safe_dump(service))
        time.sleep(2)
        service_ip = kubectl("get", "service/nke-network-matrix", "-o", "jsonpath={.spec.clusterIP}")
        for node in nodes:
            def node_service(node=node):
                answer = http(lambda *args: lab.exec(node["container"], *args), service_ip)
                require(answer["hostname"] in pod_names, "Service reached an unexpected endpoint")
                return answer
            record(role, "node-to-Service", node["ip"], service_ip, node_service)
        for a in pod_names[:-1]:
            def pod_services(a=a):
                script = """import json,socket,ssl,urllib.request
from pathlib import Path
name='nke-network-matrix.default.svc.cluster.local'
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


if __name__ == "__main__":
    from managed import ManagedLab
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", type=Path, required=True)
    args = parser.parse_args()
    verify_networks(ManagedLab(args.state.resolve()))
