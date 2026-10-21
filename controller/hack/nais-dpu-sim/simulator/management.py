# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Bootstrap the isolated NVKE management cluster."""
import json
import os
import secrets

import yaml

from .config import REPO, RUNTIME_IMAGE, PROVIDER_IMAGE, TEMPORAL_IMAGE, POSTGRES_IMAGE
from .runtime import command, wait_until


def management_up(sim, provider_chart):
    kubeconfig = sim.state / "management.kubeconfig"
    manager = cluster_up(sim)
    archive = sim.state / "management-images.tar"
    command("docker", "save", "-o", str(archive), RUNTIME_IMAGE, PROVIDER_IMAGE, TEMPORAL_IMAGE, POSTGRES_IMAGE)
    command("docker", "cp", str(archive), manager + ":/var/lib/management-images.tar")
    sim.exec(manager, "ctr", "-n", "k8s.io", "images", "import", "/var/lib/management-images.tar")
    sim.exec(manager, "rm", "/var/lib/management-images.tar")
    command("clusterctl", "init", "--core", "cluster-api:v1.12.3", "--bootstrap", "kubeadm:v1.12.3",
            "--control-plane", "kubeadm:v1.12.3", "--kubeconfig", str(kubeconfig))
    command("helm", "upgrade", "--install", "capi-provider-nico", str(provider_chart), "--kubeconfig", str(kubeconfig),
            "--namespace", "capnico-system", "--create-namespace", "--set-string", "manager.image.repository=cluster-api-provider-nico",
            "--set-string", "manager.image.tag=local", "--wait", "--timeout", "5m")
    sim.mgmt("apply", "-f", "-", input=yaml.safe_dump({"apiVersion": "v1", "kind": "Secret",
        "metadata": {"name": "nico-credentials", "namespace": "capnico-system"},
        "stringData": {"endpoint": sim.config["nico_endpoint"], "orgID": "novxwn552ssc", "apiName": "forge", "token": "example-token"}}))
    ns = {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "nvke-nais-dpu-sim"}}
    sim.mgmt("apply", "-f", "-", input=yaml.safe_dump(ns))
    sim.mgmt("apply", "-f", "-", input=yaml.safe_dump({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "nke-system"}}))
    password = secrets.token_urlsafe(32)
    db_secret = {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "nvke-nais-dpu-sim-db", "namespace": "nvke-nais-dpu-sim"},
                 "stringData": {"password": password}}
    try:
        sim.mgmt("get", "secret/nvke-nais-dpu-sim-db", "-n", "nvke-nais-dpu-sim", "-o", "name")
        db_secret = None
    except RuntimeError as err:
        if "NotFound" not in str(err):
            raise
    password_ref = {"secretKeyRef": {"name": "nvke-nais-dpu-sim-db", "key": "password"}}
    account = {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "nvke-controller", "namespace": "nvke-nais-dpu-sim"}}
    binding = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": {"name": "nvke-nais-dpu-sim-controller"},
               "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "cluster-admin"},
               "subjects": [{"kind": "ServiceAccount", "name": "nvke-controller", "namespace": "nvke-nais-dpu-sim"}]}
    postgres = deployment("postgres", POSTGRES_IMAGE,
        [{"name": "POSTGRES_DB", "value": "nvke"}, {"name": "POSTGRES_PASSWORD", "valueFrom": password_ref}],
        data_path="/var/local/nvke-nais-dpu-sim/postgres", port=5432)
    temporal = deployment("temporal", TEMPORAL_IMAGE, args=["server", "start-dev", "--ip", "0.0.0.0", "--db-filename", "/data/temporal.db"],
                          data_path="/var/local/nvke-nais-dpu-sim/temporal", port=7233)
    # hostPath directories are created by root; the dev Temporal image runs
    # as a non-root user and needs a writable SQLite directory.
    sim.exec(manager, "mkdir", "-p", "/var/local/nvke-nais-dpu-sim/temporal")
    sim.exec(manager, "chown", "1000:1000", "/var/local/nvke-nais-dpu-sim/temporal")
    controller_config = yaml.safe_load((REPO / "controller/hack/local-controller-config.yaml").read_text())
    controller_config.update(installCNIAfterInit=True, supportedInfraProviders=["infra"], nicoControllersEnabled=False,
                             profilesDir="/profiles", runbooksDir="")
    controller_config["temporal"].update(hostPort="temporal:7233", namespace="default")
    configuration = {"apiVersion": "v1", "kind": "ConfigMap",
                     "metadata": {"name": "nvke-controller-config", "namespace": "nvke-nais-dpu-sim"},
                     "data": {"config.yaml": yaml.safe_dump(controller_config)}}
    env = {"DB_CONNECTION_STRING": "postgres://postgres:REDACTED@postgres:5432/nvke?sslmode=disable",
           "CONTROLLER_CONFIG_PATH": "/etc/nvke/controller/config.yaml", "NODE_AGENT_PORT": "15005",
           "LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP": "true", "LOG_LEVEL": "info"}
    controller_env = [{"name": k, "value": v} for k, v in env.items()]
    controller_env.append({"name": "CNPG_SUPERUSER_PASSWORD", "valueFrom": password_ref})
    controller_env.append({"name": "POD_NAMESPACE", "valueFrom": {"fieldRef": {"fieldPath": "metadata.namespace"}}})
    controller = deployment("nvke-controller", RUNTIME_IMAGE, controller_env, account="nvke-controller", port=8080)
    controller["spec"]["template"]["spec"]["containers"][0]["command"] = ["controller"]
    controller["spec"]["template"]["spec"]["volumes"] = [{"name": "configuration", "configMap": {"name": "nvke-controller-config"}}]
    controller["spec"]["template"]["spec"]["containers"][0]["volumeMounts"] = [
        {"name": "configuration", "mountPath": "/etc/nvke/controller", "readOnly": True}]

    objects = [configuration, account, binding, postgres, temporal, controller,
               service("postgres", 5432), service("temporal", 7233), service("nvke-controller", 8080, 30080)]
    if db_secret:
        objects.insert(0, db_secret)
    sim.mgmt("apply", "-f", "-", input=yaml.safe_dump_all(objects))
    sim.mgmt("rollout", "status", "deployment/nvke-controller", "-n", "nvke-nais-dpu-sim", "--timeout=300s")
    wait_until("NVKE REST API", lambda: sim.nvke("GET", "/version"), 120)
    sim.config["management_ready"] = True
    sim.save()
    print("NVKE controller, Temporal, PostgreSQL, CAPI and CAPNICo are running in the management cluster", flush=True)


def cluster_up(sim):
    """Use the shared Kind setup with a run-local kubeconfig and API port."""
    kubeconfig = sim.state / "management.kubeconfig"
    kind_config = yaml.safe_load((REPO / "controller/hack/controller-capi-kind.yaml").read_text())
    control_plane = next(node for node in kind_config["nodes"] if node["role"] == "control-plane")
    control_plane.setdefault("extraPortMappings", []).append(
        {"containerPort": 30080, "hostPort": sim.config["controller_port"], "listenAddress": "127.0.0.1"})
    config_path = sim.state / "management-kind.yaml"
    config_path.write_text(yaml.safe_dump(kind_config))
    env = {**os.environ, "KUBECONFIG": str(kubeconfig)}
    # Kind owns its default dual-stack Docker network. The simulation network
    # is attached separately so the controller and CAPNICo can reach the hosts.
    env.pop("KIND_EXPERIMENTAL_DOCKER_NETWORK", None)
    command("make", "-C", str(REPO / "controller"), "kind-up",
            "KIND_CLUSTER_NAME=" + sim.config["management_cluster"], "KIND_CLUSTER_CONFIG=" + str(config_path),
            # Preserve the Docker context selected by Simulation on macOS too.
            "kind-docker-host=", env=env)
    kubeconfig.chmod(0o600)
    manager = sim.config["management_cluster"] + "-control-plane"
    networks = json.loads(command("docker", "inspect", manager))[0]["NetworkSettings"]["Networks"]
    if sim.name not in networks:
        command("docker", "network", "connect", sim.name, manager)
    return manager


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
    return {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": name, "namespace": "nvke-nais-dpu-sim"},
            "spec": {"replicas": 1, "selector": {"matchLabels": {"app": name}},
                     "template": {"metadata": {"labels": {"app": name}}, "spec": pod}}}


def service(name, port, node_port=None):
    spec = {"selector": {"app": name}, "ports": [{"port": port, "targetPort": port}]}
    if node_port:
        spec["type"] = "NodePort"
        spec["ports"][0]["nodePort"] = node_port
    return {"apiVersion": "v1", "kind": "Service", "metadata": {"name": name, "namespace": "nvke-nais-dpu-sim"}, "spec": spec}
