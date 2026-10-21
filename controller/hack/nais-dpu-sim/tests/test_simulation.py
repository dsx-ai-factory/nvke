# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Offline regression tests for nvke-nais-dpu-sim orchestration and component isolation."""
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch

import yaml

from simulator import cli, clusters, environment, fabric, lifecycle, management, dpu_workload_network, resume, scale
from simulator.config import INPUTS, REPO, WORKLOAD_ROLE


class ManagementClusterTest(unittest.TestCase):
    def test_shared_kind_configuration_retains_defaults_and_isolates_access(self):
        with tempfile.TemporaryDirectory() as directory:
            sim, manager, command = self.bootstrap(directory, {"kind": {}})
            actual = yaml.safe_load((sim.state / "management-kind.yaml").read_text())
            source = yaml.safe_load((REPO / "controller/hack/controller-capi-kind.yaml").read_text())
            control_plane = next(node for node in actual["nodes"] if node["role"] == "control-plane")
            self.assertEqual(control_plane["extraPortMappings"].pop(),
                             {"containerPort": 30080, "hostPort": 18081, "listenAddress": "127.0.0.1"})
            if not control_plane["extraPortMappings"]:
                del control_plane["extraPortMappings"]
            self.assertEqual(actual, source)
            make = command.call_args_list[0]
            self.assertEqual(make.args[:4], ("make", "-C", str(REPO / "controller"), "kind-up"))
            self.assertIn("KIND_CLUSTER_NAME=nvke-nais-dpu-sim-example-mgmt", make.args)
            self.assertIn("kind-docker-host=", make.args)
            self.assertEqual(make.kwargs["env"]["DOCKER_CONTEXT"], "example-daemon")
            self.assertEqual(make.kwargs["env"]["KUBECONFIG"], str(sim.state / "management.kubeconfig"))
            self.assertNotIn("KIND_EXPERIMENTAL_DOCKER_NETWORK", make.kwargs["env"])
            self.assertEqual((sim.state / "management.kubeconfig").stat().st_mode & 0o777, 0o600)
            command.assert_any_call("docker", "network", "connect", sim.name, manager)

    def test_existing_network_attachment_is_preserved_on_resume(self):
        with tempfile.TemporaryDirectory() as directory:
            _, _, command = self.bootstrap(directory, {"kind": {}, "nvke-nais-dpu-sim-example": {}})
            self.assertFalse(any(call.args[:3] == ("docker", "network", "connect") for call in command.call_args_list))

    def test_kind_failure_does_not_start_component_installation(self):
        with tempfile.TemporaryDirectory() as directory:
            sim = SimpleNamespace(state=Path(directory), name="example",
                                  config={"management_cluster": "example-mgmt", "controller_port": 18081})
            with patch.object(management, "command", side_effect=RuntimeError("kind failed")) as command:
                with self.assertRaisesRegex(RuntimeError, "kind failed"):
                    management.management_up(sim, Path("/example/chart"))
            self.assertEqual(command.call_count, 1)

    def bootstrap(self, directory, networks):
        sim = SimpleNamespace(state=Path(directory), name="nvke-nais-dpu-sim-example",
                              config={"management_cluster": "nvke-nais-dpu-sim-example-mgmt", "controller_port": 18081})

        def run(*args, **kwargs):
            if args[0] == "make":
                Path(kwargs["env"]["KUBECONFIG"]).write_text("example kubeconfig")
            if args[:2] == ("docker", "inspect"):
                return json.dumps([{"NetworkSettings": {"Networks": networks}}])
            return ""

        with patch.object(management, "command", side_effect=run) as command, \
             patch.dict(os.environ, {"DOCKER_CONTEXT": "example-daemon", "KUBECONFIG": "/tmp/user-kubeconfig",
                                     "KIND_EXPERIMENTAL_DOCKER_NETWORK": "unrelated-network"}):
            manager = management.cluster_up(sim)
            self.assertEqual(os.environ["KUBECONFIG"], "/tmp/user-kubeconfig")
        return sim, manager, command


class ClusterRequestsTest(unittest.TestCase):
    def test_network_roles_and_gpu_selector_stay_distinct(self):
        sim = SimpleNamespace(name="nvke-nais-dpu-sim-example", config={"ids": {str(i): str(i) for i in range(1, 6)}})
        dpu = clusters.cluster_request(sim, "dpu")
        workload = clusters.cluster_request(sim, WORKLOAD_ROLE)
        self.assertEqual(dpu["cniPlugin"], "ovn-kubernetes")
        self.assertEqual(workload["cniPlugin"], "ovn-kubernetes-dpu-host")
        self.assertNotEqual(dpu["network"]["podCIDR"], workload["network"]["podCIDR"])
        self.assertNotEqual(dpu["network"]["serviceCIDR"], workload["network"]["serviceCIDR"])
        self.assertEqual(workload["nodePools"][0]["nodeLabels"], {"nodeType": "gpu"})
        self.assertNotIn("nodeLabels", dpu["nodePools"][0])
        for request in (dpu, workload):
            self.assertEqual(request["cniVersion"], INPUTS["ovn"]["version"])


class ProvisioningTest(unittest.TestCase):
    def test_new_dpu_cluster_records_nodes_without_refreshing_saved_records(self):
        control_plane = {"node_id": "cp", "name": "control-plane", "container": "cp-container"}
        worker = {"node_id": "worker", "name": "worker", "container": "worker-container"}
        saved_nodes = []
        sim = SimpleNamespace(config={"cluster_ids": {}, "dpu_nodes": saved_nodes}, dpus=saved_nodes, save=MagicMock())
        with patch.object(clusters, "provision") as provision, \
             patch.object(clusters, "discover", side_effect=[[control_plane], [control_plane, worker]]) as discover, \
             patch("simulator.runtime.time.sleep"), \
             patch.object(resume, "refresh_saved_dpu_nodes") as refresh, \
             patch.object(lifecycle, "configure_dpu_cluster") as configure:
            lifecycle.prepare_dpu_cluster(sim)
        provision.assert_called_once_with(sim, "dpu")
        self.assertEqual(discover.call_count, 2)
        self.assertIs(sim.dpus, saved_nodes)
        self.assertEqual(saved_nodes, [control_plane, worker])
        sim.save.assert_called_once_with()
        refresh.assert_not_called()
        configure.assert_called_once_with(sim)

    def test_resume_creates_only_clusters_without_saved_ids(self):
        for role, resume_cluster, prepare, configure in [
            ("dpu", resume.resume_dpu_cluster, "prepare_dpu_cluster", "configure_dpu_cluster"),
            (WORKLOAD_ROLE, resume.resume_workload_cluster, "prepare_workload_cluster", "configure_workload_cluster"),
        ]:
            for cluster_exists in [False, True]:
                with self.subTest(role=role, cluster_exists=cluster_exists):
                    cluster_ids = {role: "saved-cluster-id"} if cluster_exists else {}
                    sim = SimpleNamespace(config={"cluster_ids": cluster_ids})
                    with patch.object(lifecycle, prepare) as create, \
                         patch.object(lifecycle, configure) as configure_cluster, \
                         patch.object(resume, "refresh_saved_dpu_nodes") as refresh:
                        resume_cluster(sim)
                    if cluster_exists:
                        create.assert_not_called()
                        configure_cluster.assert_called_once_with(sim)
                        if role == "dpu":
                            refresh.assert_called_once_with(sim)
                        else:
                            refresh.assert_not_called()
                    else:
                        create.assert_called_once_with(sim)
                        configure_cluster.assert_not_called()
                        refresh.assert_not_called()

    def test_new_workload_cluster_is_requested_before_network_setup(self):
        sim = SimpleNamespace()
        events = []
        with patch.object(clusters, "provision", side_effect=lambda sim, role: events.append(role)), \
             patch.object(lifecycle, "configure_workload_cluster", side_effect=lambda sim: events.append("networking")):
            lifecycle.prepare_workload_cluster(sim)
        self.assertEqual(events, [WORKLOAD_ROLE, "networking"])

    def test_create_prepares_environment_before_management_and_tenants(self):
        sim = SimpleNamespace(state=Path("/tmp/example-run"))
        events = []
        with patch.object(environment, "prepare", side_effect=lambda *args: events.append("environment") or sim), \
             patch.object(management, "management_up", side_effect=lambda *args: events.append("management")), \
             patch.object(lifecycle, "prepare_dpu_cluster", side_effect=lambda *args: events.append("dpu")), \
             patch.object(lifecycle, "prepare_workload_cluster", side_effect=lambda *args: events.append("workload")):
            lifecycle.create(Path("/example/chart"), "example-daemon")
        self.assertEqual(events, ["environment", "management", "dpu", "workload"])

    def test_wrong_ovn_image_allocates_no_run_resources(self):
        with tempfile.TemporaryDirectory() as directory, \
             patch.dict(os.environ), \
             patch.object(environment, "STATE", Path(directory)), \
             patch.object(environment, "chart_check"), \
             patch.object(environment, "command", return_value="wrong-image-id") as command:
            with self.assertRaisesRegex(RuntimeError, "does not identify the pinned upstream arm64 image"):
                environment.prepare(Path("/example/chart"), "example-daemon")
            self.assertEqual(list(Path(directory).iterdir()), [])
            self.assertFalse(any(call.args[:3] in [("docker", "network", "create"), ("docker", "create", "--name")]
                                 for call in command.call_args_list))

    def test_dpu_discovery_preserves_existing_pairs_during_scale(self):
        control_plane = {"node_id": "cp", "name": "control-plane", "container": "cp-container"}
        old = {"node_id": "old", "name": "z-worker", "container": "old-container", "offload_ready": True}
        added = {"node_id": "new", "name": "a-worker", "container": "new-container"}
        sim = SimpleNamespace(config={"cluster_ids": {"dpu": "cluster-id"}, "workers": 2},
                              dpus=[control_plane, old], save=MagicMock(), dpu_kubectl=MagicMock(), nvke=MagicMock())
        # Discovery sorts the newly added worker before the existing one.
        with patch.object(clusters, "discover", return_value=[control_plane, added, {**old, "ip": "192.0.2.1"}]), \
             patch.object(clusters, "ready", return_value=True), \
             patch.object(clusters, "nodes_ready", return_value=True), \
             patch.object(clusters, "export_kubeconfig"), \
             patch.object(lifecycle, "dpu_workload_network_setup"), \
             patch.object(lifecycle, "wait_until", side_effect=lambda description, check, *args: check()):
            scale.scale_dpu_cluster(sim, "dpu-workers", 2)
        self.assertEqual([n["node_id"] for n in sim.dpus], ["cp", "old", "new"])
        self.assertTrue(sim.dpus[1]["offload_ready"])
        self.assertEqual(sim.dpus[1]["ip"], "192.0.2.1")
        sim.nvke.assert_called_once_with("PATCH", "/nodePools/dpu-workers", {"desiredNodeCount": 2})

    def test_dpu_refresh_waits_for_all_nodes_and_updates_the_saved_list(self):
        control_plane = {"node_id": "cp", "name": "control-plane", "container": "cp-container"}
        worker = {"node_id": "worker", "name": "worker", "container": "worker-container"}
        saved_nodes = [{"node_id": "removed", "name": "removed-worker"}]
        sim = SimpleNamespace(config={"dpu_nodes": saved_nodes}, dpus=saved_nodes, save=MagicMock())

        with patch.object(clusters, "discover", side_effect=[[control_plane], [control_plane, worker]]) as discover, \
             patch("simulator.runtime.time.sleep"):
            scale.refresh_saved_dpu_nodes(sim)

        self.assertEqual(discover.call_count, 2)
        self.assertIs(sim.dpus, saved_nodes)
        self.assertEqual(sim.config["dpu_nodes"], [control_plane, worker])
        sim.save.assert_called_once_with()

    def test_resume_reuses_management_before_provisioning_in_order(self):
        sim = SimpleNamespace(config={"management_ready": True})
        events = []
        with patch("sys.argv", ["nais-dpu-sim.py", "resume", "--state", "/tmp/example-run"]), \
             patch.object(cli, "Simulation", return_value=sim), \
             patch.object(management, "management_up") as bootstrap, \
             patch.object(resume, "resume_dpu_cluster", side_effect=lambda sim: events.append("dpu")), \
             patch.object(resume, "resume_workload_cluster", side_effect=lambda sim: events.append("workload")):
            cli.main()
        bootstrap.assert_not_called()
        self.assertEqual(events, ["dpu", "workload"])

    def test_scale_command_uses_the_scale_module(self):
        sim = SimpleNamespace()
        with patch("sys.argv", ["nais-dpu-sim.py", "scale", "--state", "/tmp/example-run", "--workers", "2"]), \
             patch.object(cli, "Simulation", return_value=sim), \
             patch.object(scale, "scale") as scale_clusters:
            cli.main()
        scale_clusters.assert_called_once_with(sim, 2)

    def test_resume_bootstraps_management_only_when_needed(self):
        sim = SimpleNamespace(config={"management_ready": False})
        events = []
        with patch("sys.argv", ["nais-dpu-sim.py", "resume", "--state", "/tmp/example-run", "--provider-chart", "/tmp/example-chart"]), \
             patch.object(cli, "Simulation", return_value=sim), \
             patch.object(management, "management_up", side_effect=lambda *args: events.append("management")), \
             patch.object(resume, "resume_dpu_cluster", side_effect=lambda sim: events.append("dpu")), \
             patch.object(resume, "resume_workload_cluster", side_effect=lambda sim: events.append("workload")):
            cli.main()
        self.assertEqual(events, ["management", "dpu", "workload"])


class OffloadTest(unittest.TestCase):
    def test_isolation_preserves_container_entrypoint_and_probe(self):
        spec = {"containers": [{"name": "ovn", "image": "example/ovn:test", "args": ["--node", "dpu-0"],
                                "readinessProbe": {"exec": {"command": ["/health", "--ready"]}}}],
                "volumes": [{"name": "ovs", "hostPath": {"path": "/var/run/openvswitch"}}]}
        with patch.object(dpu_workload_network, "command", return_value=json.dumps([{"Config": {"Entrypoint": ["/start"]}}])):
            dpu_workload_network.isolate_pod(spec)
        container = spec["containers"][0]
        self.assertEqual(container["command"], ["ip", "netns", "exec", dpu_workload_network.NAMESPACE, "/start", "--node", "dpu-0"])
        self.assertEqual(container["readinessProbe"]["exec"]["command"],
                         ["ip", "netns", "exec", dpu_workload_network.NAMESPACE, "/health", "--ready"])
        self.assertEqual(spec["volumes"][0]["hostPath"]["path"], dpu_workload_network.RUN_DIR)
        self.assertTrue(container["securityContext"]["privileged"])
        self.assertEqual(container["volumeMounts"][0]["mountPropagation"], "HostToContainer")

    def test_frr_install_restores_container_identity_on_failure(self):
        host = {"dpu": "container-id", "dpu_node_name": "dpu-node"}
        sim = SimpleNamespace(hosts=[host])
        with patch.object(fabric, "install_components", side_effect=RuntimeError("example failure")):
            with self.assertRaisesRegex(RuntimeError, "example failure"):
                fabric.install(sim)
        self.assertEqual(host["dpu"], "container-id")


if __name__ == "__main__":
    unittest.main()
