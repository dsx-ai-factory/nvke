# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run state, command execution, and cluster access."""
import base64
import json
import os
import socket
import subprocess
import time
import urllib.error
import urllib.request
from typing import TypedDict

import yaml


class ProvisionedNode(TypedDict):
    """An NVKE node matched to its running simulation container."""

    name: str
    node_id: str
    container: str
    id: str
    ip: str


class DpuNode(ProvisionedNode, total=False):
    """A provisioned DPU with optional progress fields added during setup."""

    offload_ip: str
    offload_ready: bool
    matrix_http_pid: int


class Simulation:
    """Hold run state and access Docker, Kubernetes, and the NVKE API."""

    dpus: list[DpuNode]
    """DPU records saved during creation and refreshed during resume or scale.

    - name: node hostname.
    - node_id: NVKE node ID.
    - container: Docker container ID.
    - id: NICo instance ID.
    - ip: management IP on the simulation network.
    - offload_ip: optional eth0 IP inside workload-offload.
    - offload_ready: optional flag for completed namespace and OVS setup.
    - matrix_http_pid: optional process ID for the test HTTP server.

    A new run starts with an empty list. A resumed run loads the saved records.
    This is the same list as config["dpu_nodes"], saved to run.json by save().
    """

    def __init__(self, state):
        self.state = state
        self.config = json.loads((state / "run.json").read_text())
        self.name = self.config["name"]
        self.hosts = self.config["hosts"]
        self.dpus = self.config.setdefault("dpu_nodes", [])
        os.environ["DOCKER_CONTEXT"] = self.config["docker_context"]
        os.environ.pop("DOCKER_HOST", None)

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

    def service_account_token(self, account, namespace):
        name = account + "-nvke-nais-dpu-sim-access"
        self.apply([{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/service-account-token",
                     "metadata": {"name": name, "namespace": namespace,
                                  "annotations": {"kubernetes.io/service-account.name": account}}}])
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            secret = json.loads(self.kubectl("get", "secret", name, "-n", namespace, "-o", "json"))
            if token := secret.get("data", {}).get("token"):
                return base64.b64decode(token).decode()
            time.sleep(1)
        raise RuntimeError(f"The control plane did not populate the token for {namespace}/{account}")

    def mgmt(self, *args, **kwargs):
        return command("kubectl", "--kubeconfig", str(self.state / "management.kubeconfig"), *args, **kwargs)

    def dpu_kubectl(self, *args, **kwargs):
        require(bool(self.dpus), "DPU nodes have not been provisioned")
        return self.exec(self.dpus[0]["container"], "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", *args, **kwargs)

    def nvke(self, method, path, payload=None):
        request = urllib.request.Request(self.config["controller_api"] + path, method=method,
                                        data=json.dumps(payload).encode() if payload is not None else None,
                                        headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                data = response.read()
                return json.loads(data) if data else None
        except urllib.error.HTTPError as err:
            raise RuntimeError(f"NVKE {method} {path}: HTTP {err.code}: {err.read().decode()}") from err

    def dpu_exec(self, node, *args, **kwargs):
        from .dpu_workload_network import exec_in_dpu_workload_network

        return exec_in_dpu_workload_network(self, node, *args, **kwargs)


def command(*args, **kwargs):
    result = subprocess.run(args, text=True, capture_output=True, **kwargs)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed ({result.returncode}): {result.stderr.strip()} {result.stdout.strip()}")
    return result.stdout.strip()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


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
