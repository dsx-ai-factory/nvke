# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Offline collector regressions. These tests never contact a cluster."""
import base64
import copy
import http.server
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import threading
import time
import unittest
import urllib.parse
from unittest.mock import patch

import demo

SPEC = importlib.util.spec_from_file_location("receiver", Path(__file__).with_name("alert-receiver.py"))
receiver = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(receiver)


def config():
    c = json.loads(Path(__file__).with_name("config.example.json").read_text())
    c["timeout_seconds"] = 10
    c["deployment_source_revision"] = demo.command(["git", "-C", str(demo.SOURCE_ROOT), "rev-parse", "HEAD"]).decode().strip()
    return c


def snapshot(c, changed=False):
    items = {}
    for n in c["nodes"]:
        for name in demo.CATALOG:
            if name == "super-admin":
                continue
            is_leaf = name in demo.LEAF
            items[(n["id"], name)] = {"sha256": demo.digest(name + n["id"] + ("changed" if changed and is_leaf else "original")),
                                      "not_after": "2028-01-01T00:00:00Z" if changed and is_leaf else "2027-01-01T00:00:00Z"}
    return items


class FakeLive:
    failure = None
    def __init__(self, c):
        self.c, self.started, self.owner = c, False, "prior"
    def identity(self):
        if self.failure == "identity":
            raise demo.CheckError("kubernetes_target_mismatch")
    def health(self):
        if self.failure == "health":
            raise demo.CheckError("etcd_endpoint_unhealthy")
        key = "after" if self.started and self.c["scenario"] != "etcd-key" else "before"
        return {"members": ["1", "2", "3"], "components": {(n["name"], component): key for n in self.c["nodes"] for component in demo.COMPONENTS}}
    def versions(self, _manual=False):
        return [{"component": "controller", "reported_version_sha256": "a" * 64}]
    def certificates(self, after=0, manual=False):
        if self.failure == "freshness" and after:
            raise demo.CheckError("certificate_inspection_stale")
        return snapshot(self.c, self.started)
    def cluster(self):
        state = {"cycleID": self.owner, "state": "succeeded", "outcome": "renewed"}
        return {"id": self.c["cluster_id"], "name": self.c["cluster_name"], "conditions": {"certificateRenewal": state,
                "provisioned": {"status": "True"}}, "etcdEncrypted": True}
    def metrics(self, _certificates, after=0):
        if self.failure == "metrics":
            raise demo.CheckError("inspection_metric_unavailable")
    def temporal(self, action, owner, *args):
        self.started, self.owner = True, owner
        return {}
    def workflow(self, _owner, _expected_type):
        if self.failure == "workflow":
            raise demo.CheckError("workflow_failed")
        return "COMPLETED"
    def serving_certificates(self, _snapshot):
        if self.failure == "serving":
            raise demo.CheckError("installed_certificate_not_served")
        return []
    def audit(self, *_args):
        if self.failure == "audit":
            raise demo.CheckError("audit_coverage_incomplete")
        return []
    def kubectl(self, *_args):
        return b"public fixture"


class FakeTraffic:
    def __init__(self, _live, _proof=None):
        self.thread = self
    def start(self):
        pass
    def finish(self):
        return {"samples": 10, "errors": 0, "max_latency_seconds": 0.1}


class SampledPrometheus:
    """Offline instant-vector lookback: values stay frozen; timestamp returns scrape time."""
    def __init__(self, c):
        self.c, self.series, self.queries = c, {}, []
        self.stored = 3

    def put(self, suffix, labels, sampled_at, value):
        labels = {"__name__": "nvke_controller_" + suffix, "tenant_cluster": self.c["cluster_name"], **labels}
        self.series.setdefault(tuple(sorted(labels.items())), []).append((sampled_at, value))

    def scrape(self, certificates, sampled_at, inspected_at, csr_observed_at, instance="controller-1"):
        for n in self.c["nodes"]:
            labels = {"node": n["name"], "instance": instance}
            self.put("certificate_inspection_available", labels, sampled_at, 1)
            self.put("certificate_observation_age_seconds", labels, sampled_at, sampled_at - inspected_at)
            for (node_id, credential), certificate in certificates.items():
                if node_id == n["id"]:
                    identity = {**labels, "credential": credential}
                    self.put("certificate_not_after_timestamp_seconds", identity, sampled_at, int(demo.timestamp(certificate["not_after"])))
                    self.put("certificate_observation_available", identity, sampled_at, 1)
        self.put("serving_csr_observation_age_seconds", {"instance": instance}, sampled_at, sampled_at - csr_observed_at)

    def http(self, _base, suffix, _auth_key=None):
        if suffix.startswith("/api/v1/rules"):
            return {"status": "success", "data": {"groups": [{"rules": [{"name": name, "health": "ok"} for name in
                    ("NVKECertificateExpiring", "NVKECertificateRenewalFailed", "NVKEEtcdKeyRotationStuck")]}]}}
        parameters = urllib.parse.parse_qs(urllib.parse.urlsplit(suffix).query)
        expression = parameters["query"][0]
        evaluated_at = float(parameters["time"][0]) if "time" in parameters else time.time()
        self.queries.append((expression, evaluated_at))
        result = []
        if expression.startswith("sum("):
            if 'result="stored"' in expression and self.stored:
                result = [{"metric": {}, "value": [evaluated_at, str(self.stored)]}]
        else:
            branches = [(expression, None)]
            if expression.startswith("label_replace("):
                branches = []
                for branch in expression.split(" or "):
                    selector, replacement = branch[len("label_replace("):].split(', "nvke_demo_sample", ', 1)
                    kind = json.loads("[" + replacement[:-1] + "]")[0]
                    branches.append((selector, kind))
            for selector, kind in branches:
                source_timestamp = selector.startswith("timestamp(")
                selector = selector[len("timestamp("):-1] if source_timestamp else selector
                name = selector.split("{", 1)[0]
                for identity, history in self.series.items():
                    labels = dict(identity)
                    if labels["__name__"] != name:
                        continue
                    eligible = [(sampled, value) for sampled, value in history if evaluated_at - 300 <= sampled <= evaluated_at]
                    if not eligible:
                        continue
                    sampled, value = max(eligible, key=lambda point: point[0])
                    if source_timestamp:
                        labels.pop("__name__")
                    if kind:
                        labels["nvke_demo_sample"] = kind
                    result.append({"metric": labels, "value": [evaluated_at, str(sampled if source_timestamp else value)]})
        return {"status": "success", "data": {"resultType": "vector", "result": result}}


class DemoTests(unittest.TestCase):
    def run_fake(self, failure=None, approve=True):
        c = config()
        FakeLive.failure = failure
        with patch.object(demo, "Traffic", FakeTraffic), patch.object(demo, "wait_for", lambda check, _deadline: check()):
            return demo.run(c, "forced-renewal", c["cluster_id"] if approve else None, FakeLive)

    def test_success_contains_public_comparisons_and_checked_workflow(self):
        result = self.run_fake()
        self.assertEqual("passed", result["status"])
        self.assertTrue(result["mutation_started"])
        self.assertIn("terminal_workflow_and_owning_state_success", result["checks"])
        self.assertEqual(demo.required_checks("forced-renewal"), set(result["checks"]))
        self.assertEqual(45, len(result["certificates_before"]))
        self.assertNotEqual(result["certificates_before"], result["certificates_after"])

    def test_each_required_failure_fails_without_false_pass(self):
        for failure in ("identity", "health", "metrics", "workflow", "freshness", "serving", "audit"):
            with self.subTest(failure=failure):
                result = self.run_fake(failure)
                self.assertEqual("failed", result["status"])
                self.assertIn("reason", result)

    def test_sticky_update_failed_is_refused_before_any_mutation(self):
        class Failed(FakeLive):
            def cluster(self):
                result = super().cluster()
                result["conditions"]["updateFailed"] = {"status": "True"}
                return result
        c, created = config(), []
        FakeLive.failure = None
        def factory(config):
            created.append(Failed(config))
            return created[0]
        with patch.object(demo, "Traffic", FakeTraffic), patch.object(demo, "wait_for", lambda check, _deadline: check()):
            result = demo.run(c, "forced-renewal", c["cluster_id"], factory)
        self.assertEqual("update_failed_present", result["reason"])
        self.assertFalse(result["mutation_started"])
        self.assertFalse(created[0].started)

    def test_approval_is_required_before_temporal_mutation(self):
        result = self.run_fake(approve=False)
        self.assertEqual("operator_approval_required", result["reason"])
        self.assertFalse(result["mutation_started"])

    def test_command_failure_timeout_and_raw_diagnostics_are_redacted(self):
        sentinel = "PRIVATE_KEY_TOKEN_KUBECONFIG_SENTINEL"
        with self.assertRaises(demo.CheckError) as error:
            demo.command([sys.executable, "-c", "import sys; print('" + sentinel + "'); print('" + sentinel + "',file=sys.stderr);sys.exit(3)"])
        self.assertEqual("command_failed", str(error.exception))
        self.assertNotIn(sentinel, str(error.exception))
        with self.assertRaisesRegex(demo.CheckError, "timed_out"):
            demo.command([sys.executable, "-c", "import time; time.sleep(1)"], timeout=0.01)

    def test_failed_workflow_is_terminal_and_timeout_never_success(self):
        with self.assertRaisesRegex(demo.CheckError, "workflow_failed"):
            demo.wait_for(lambda: demo.require(False, "workflow_failed"), time.monotonic() + 60)
        with self.assertRaisesRegex(demo.CheckError, "observation_incomplete"):
            demo.wait_for(lambda: True, time.monotonic() - 1)

    def test_finished_unchanged_cycle_fails_at_once_instead_of_polling_to_timeout(self):
        calls = []
        def finished_unchanged():
            calls.append(1)
            demo.require(False, "scheduled_cycle_did_not_renew")
        with patch.object(demo.time, "sleep", side_effect=AssertionError("must not retry")):
            with self.assertRaisesRegex(demo.CheckError, "scheduled_cycle_did_not_renew"):
                demo.wait_for(finished_unchanged, time.monotonic() + 60)
        self.assertEqual(1, len(calls))

    def test_expiry_fingerprint_and_ca_invariants(self):
        c = config()
        before, after = snapshot(c), snapshot(c, True)
        demo.compare_certificates(before, after, True)
        for mutation, reason in (("ca", "ca_certificate_changed"), ("leaf", "leaf_certificate_not_renewed")):
            broken = copy.deepcopy(after)
            key = (c["nodes"][0]["id"], "kubernetes-ca" if mutation == "ca" else "apiserver")
            broken[key] = before[key] if mutation == "leaf" else after[(key[0], "apiserver")]
            with self.assertRaisesRegex(demo.CheckError, reason):
                demo.compare_certificates(before, broken, True)

    def test_traffic_error_budget_is_required(self):
        class BadTraffic(FakeTraffic):
            def finish(self):
                return {"samples": 10, "errors": 1, "max_latency_seconds": 0.1}
        c = config()
        FakeLive.failure = None
        with patch.object(demo, "Traffic", BadTraffic), patch.object(demo, "wait_for", lambda check, _deadline: check()):
            result = demo.run(c, "forced-renewal", c["cluster_id"], FakeLive)
        self.assertEqual("api_traffic_budget_exceeded", result["reason"])

    def test_webhook_strips_annotations_urls_unknown_labels_and_ids(self):
        secret = "SECRET_SENTINEL"
        body = {"externalURL": secret, "alerts": [{"status": "firing", "fingerprint": "0123456789abcdef", "generatorURL": secret,
                 "annotations": {"description": secret}, "labels": {"alertname": "NVKECertificateExpiring", "tenant_cluster": secret,
                   "node": secret, "credential": "apiserver", "secret": secret}}]}
        rows = receiver.receipts(body)
        self.assertEqual(1, len(rows))
        self.assertNotIn(secret, json.dumps(rows))
        self.assertEqual(demo.digest(secret), rows[0]["cluster_hash"])
        body["alerts"][0]["labels"]["credential"] = secret
        self.assertEqual([], receiver.receipts(body))

    def test_receiver_missing_resolution_and_untrusted_receipt_fail(self):
        c = config()
        with tempfile.TemporaryDirectory() as tmp:
            c["alert_receipts_file"] = str(Path(tmp) / "receipts")
            common = {"schema": "nke.alert-receipt.v1", "cluster_hash": demo.digest(c["cluster_name"]),
                      "node_hash": demo.digest(c["alert_node"]), "credential": c["alert_credential"], "received_at": demo.now(),
                      "receipt_id": "a" * 64}
            Path(c["alert_receipts_file"]).write_text(json.dumps({**common, "status": "firing"}) + "\n")
            with self.assertRaisesRegex(demo.CheckError, "delivery_incomplete"):
                demo.alert_receipts(c, time.time() - 10, time.time() + 1, time.time() - 1)
            with Path(c["alert_receipts_file"]).open("a") as out:
                out.write(json.dumps({**common, "status": "resolved", "receipt_id": "SECRET_SENTINEL"}) + "\n")
            with self.assertRaisesRegex(demo.CheckError, "receipt_invalid"):
                demo.alert_receipts(c, time.time() - 10, time.time() + 1, time.time() - 1)

    def test_missing_scenarios_and_offline_reports_cannot_complete_release(self):
        report = self.run_fake()
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "report.json"
            demo.write_json(path, report)
            release = demo.aggregate([path])
            self.assertEqual("incomplete", release["status"])
            self.assertIn("manual-t1", release["missing_scenarios"])
            report["checks"] = []
            path.write_text(json.dumps(report))
            with self.assertRaisesRegex(demo.CheckError, "checks_missing"):
                demo.aggregate([path])

    def test_fake_collector_reports_are_not_live_and_cannot_pass_release(self):
        fake = self.run_fake()
        self.assertIs(False, fake["live"])
        with tempfile.TemporaryDirectory() as tmp:
            def aggregate_status(live):
                paths = []
                for scenario in demo.SCENARIOS:
                    path = Path(tmp) / (str(live) + "-" + scenario + ".json")
                    demo.write_json(path, {**fake, "scenario": scenario, "live": live, "source_dirty": False,
                                           "checks": sorted(demo.required_checks(scenario))})
                    paths.append(path)
                return demo.aggregate(paths)
            fake_release = aggregate_status(fake["live"])
            self.assertEqual("incomplete", fake_release["status"])
            self.assertEqual([], fake_release["missing_scenarios"])
            self.assertEqual("passed", aggregate_status(True)["status"])

    def test_release_requires_every_scenario_check_and_complete_reports_qualify(self):
        base = {"target_identity_and_health", "declared_deployed_versions_match", "public_certificate_inventory_before",
                "leaf_expiry_fingerprints_and_unchanged_cas", "nodes_ready_exact_etcd_membership_all_endpoints_healthy",
                "required_audit_actions_observed", "continuous_api_traffic_within_declared_budget"}
        automatic = {"terminal_workflow_and_owning_state_success", "fresh_metrics_and_loaded_alert_rules", "fresh_metrics_after_mutation",
                     "serving_certificate_installed_and_served"}
        renewed = automatic | {"renewed_static_pod_components_replaced"}
        expected = {
            "forced-renewal": base | renewed,
            "scheduled-renewal": base | renewed | {"scheduled_per_node_renewal_outcomes_observed"},
            "alert-renewal": base | renewed | {"expiry_alert_firing_before_mutation", "expiry_alert_resolved_and_notifications_received"},
            "etcd-key": base | automatic | {"preexisting_secret_readable_encrypted", "old_secret_rewritten_readable_and_api_primary_convergence"},
            "manual-t1": base | {"renewed_static_pod_components_replaced"},
        }
        def aggregate_reports(checks_by_scenario):
            with tempfile.TemporaryDirectory() as tmp:
                paths = []
                for scenario, checks in checks_by_scenario.items():
                    path = Path(tmp) / (scenario + ".json")
                    path.write_text(json.dumps({"schema": "nke.rotation-evidence.v1", "scenario": scenario, "status": "passed", "live": True,
                                                "source_dirty": False, "source_revision": "rev", "cluster_id": "cluster", "kube_system_uid": "uid",
                                                "profile": "profile", "checks": sorted(checks)}))
                    paths.append(path)
                return demo.aggregate(paths)
        self.assertEqual(set(demo.SCENARIOS), set(expected))
        self.assertEqual("passed", aggregate_reports(expected)["status"])
        for scenario, checks in expected.items():
            self.assertEqual(checks, demo.required_checks(scenario))
            for missing in sorted(checks):
                with self.subTest(scenario=scenario, missing=missing):
                    reports = {**expected, scenario: checks - {missing}}
                    with self.assertRaisesRegex(demo.CheckError, "evidence_checks_missing"):
                        aggregate_reports(reports)

    def test_evidence_file_exclusive_and_private(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            demo.write_json(path, {})
            self.assertEqual(0o600, path.stat().st_mode & 0o777)
            with self.assertRaises(FileExistsError):
                demo.write_json(path, {})

    def test_output_collision_rejected_before_any_live_collection(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "existing.json"
            path.write_text("public-existing-evidence")
            with patch.object(demo, "run") as run:
                status = demo.main(["run", "--config", "not-read.json", "--scenario", "forced-renewal", "--output", str(path)])
            self.assertEqual(1, status)
            run.assert_not_called()
            self.assertEqual("public-existing-evidence", path.read_text())

    def test_source_provenance_uses_demo_worktree_from_another_directory(self):
        old = os.getcwd()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                os.chdir(tmp)
                result = self.run_fake()
        finally:
            os.chdir(old)
        self.assertEqual("passed", result["status"])
        self.assertEqual(config()["deployment_source_revision"], result["source_revision"])

    def test_old_resolution_or_different_alert_id_does_not_prove_this_run(self):
        c = config()
        start = time.time()
        def iso(seconds):
            return demo.dt.datetime.fromtimestamp(seconds, demo.dt.timezone.utc).isoformat()
        common = {"schema": "nke.alert-receipt.v1", "cluster_hash": demo.digest(c["cluster_name"]),
                  "node_hash": demo.digest(c["alert_node"]), "credential": c["alert_credential"], "receipt_id": "a" * 64}
        firing = {**common, "status": "firing", "received_at": iso(start - 5)}
        with tempfile.TemporaryDirectory() as tmp:
            c["alert_receipts_file"] = str(Path(tmp) / "receipts")
            for change in ({"received_at": iso(start - 4)}, {"receipt_id": "b" * 64}):
                resolved = {**common, "status": "resolved", "received_at": iso(start + 5), **change}
                Path(c["alert_receipts_file"]).write_text(json.dumps(firing) + "\n" + json.dumps(resolved) + "\n")
                with self.assertRaisesRegex(demo.CheckError, "delivery_incomplete"):
                    demo.alert_receipts(c, start - 10, start, start + 2)
            resolved = {**common, "status": "resolved", "received_at": iso(start + 5)}
            Path(c["alert_receipts_file"]).write_text(json.dumps(firing) + "\n" + json.dumps(resolved) + "\n")
            self.assertEqual(["firing", "resolved"], [e["status"] for e in demo.alert_receipts(c, start - 10, start, start + 2)])

    def test_resolution_between_mutation_and_completion_qualifies_unless_the_alert_refires(self):
        c = config()
        start = time.time()
        def iso(seconds):
            return demo.dt.datetime.fromtimestamp(seconds, demo.dt.timezone.utc).isoformat()
        common = {"schema": "nke.alert-receipt.v1", "cluster_hash": demo.digest(c["cluster_name"]),
                  "node_hash": demo.digest(c["alert_node"]), "credential": c["alert_credential"], "receipt_id": "a" * 64}
        mutation_at, completed_at = start, start + 20
        firing = {**common, "status": "firing", "received_at": iso(start - 5)}
        resolved = {**common, "status": "resolved", "received_at": iso(start + 10)}
        with tempfile.TemporaryDirectory() as tmp:
            c["alert_receipts_file"] = str(Path(tmp) / "receipts")
            Path(c["alert_receipts_file"]).write_text(json.dumps(firing) + "\n" + json.dumps(resolved) + "\n")
            pair = demo.alert_receipts(c, start - 10, mutation_at, completed_at)
            self.assertEqual(["firing", "resolved"], [e["status"] for e in pair])
            refired = {**common, "status": "firing", "received_at": iso(start + 15)}
            with Path(c["alert_receipts_file"]).open("a") as out:
                out.write(json.dumps(refired) + "\n")
            with self.assertRaisesRegex(demo.CheckError, "notification_refired_before_completion"):
                demo.alert_receipts(c, start - 10, mutation_at, completed_at)

    def test_continuous_secret_failure_is_distinct_from_allowed_api_outage(self):
        class LocalLive:
            c = {"traffic_interval_seconds": 0.001}
            def kubectl(self, *_args):
                raise demo.CheckError("api_unavailable")
        class CorruptProof:
            calls = 0
            def read_all(self):
                self.calls += 1
                if self.calls >= 2:
                    traffic.stop.set()
                raise demo.CheckError("continuous_secret_read_or_payload_failed")
        proof = CorruptProof()
        traffic = demo.Traffic(LocalLive(), proof)
        traffic.thread.start()
        traffic.thread.join(5)
        summary = traffic.finish()
        self.assertEqual(2, summary["errors"])
        self.assertEqual(2, summary["secret_read_errors"])
        # Even a deliberately tolerant single-CP API budget cannot hide storage corruption.
        class EtcdLive(FakeLive):
            def certificates(self, after=0, manual=False):
                return snapshot(self.c)
            def cluster(self):
                cluster = super().cluster()
                cluster["conditions"]["etcdKeyRotation"] = {"workflowID": self.owner, "state": "succeeded"}
                return cluster
        class Proof:
            name = "public-fixture"
            def __init__(self, _live):
                pass
            def create(self):
                pass
            def verify(self, old=None):
                return demo.digest("new" if old else "old")
        class CorruptTraffic(FakeTraffic):
            def finish(self):
                return {"samples": 10, "errors": 1, "secret_read_errors": 1, "max_latency_seconds": 0.1}
        c = config()
        c["max_api_error_fraction"] = 1
        FakeLive.failure = None
        with patch.object(demo, "Traffic", CorruptTraffic), patch.object(demo, "EtcdProof", Proof), \
                patch.object(demo, "wait_for", lambda check, _deadline: check()):
            report = demo.run(c, "etcd-key", c["cluster_id"], EtcdLive)
        self.assertEqual("failed", report["status"])
        self.assertEqual("continuous_secret_read_or_payload_failed", report["reason"])

    def test_traffic_finish_waits_for_in_flight_observation_batch(self):
        reached, release = threading.Event(), threading.Event()
        class LocalLive:
            c = {"traffic_interval_seconds": 0.001, "nodes": [{"name": "cp-0", "control_plane": True}]}
            def __init__(self):
                self.requests, self.reads = 0, []
            def kubectl(self, *_args):
                self.requests += 1
                if self.requests == 2:
                    traffic.stop.set()
                return b"{}"
            def kubejson(self, *args):
                if args[0] == "get":
                    return {"items": [{"spec": {"nodeName": "cp-0"}, "status": {"podIP": "192.0.2.1"}}]}
                if self.requests == 2:
                    self.reads.append(args[0])
                    reached.set()
                    release.wait(5)
                return {"data": {"probe": base64.b64encode(b"nvke-rotation-public-fixture").decode()}}
        live = LocalLive()
        traffic = demo.Traffic(live, demo.EtcdProof(live))
        real_join, joins = traffic.thread.join, []
        def join(timeout=None):
            # A deadline is virtualized to 10 ms instead of sleeping; no deadline waits for the in-flight batch.
            joins.append(timeout)
            if timeout is not None:
                return real_join(0.01)
            release.set()
            return real_join(5)
        traffic.thread.start()
        self.assertTrue(reached.wait(5))
        try:
            with patch.object(traffic.thread, "join", side_effect=join):
                summary = traffic.finish()
        finally:
            release.set()
            real_join(5)
        self.assertEqual([None], joins)
        self.assertFalse(traffic.thread.is_alive())
        self.assertEqual(2, summary["samples"])
        self.assertEqual(0, summary["secret_read_errors"])


class ScheduledRenewalTests(unittest.TestCase):
    def run_scheduled(self, changed_nodes=(0,), restarted_nodes=(0,), audit_change=None,
                      owner_outcome="renewed", scenario="scheduled-renewal", certificate_change=None):
        c = config()
        clock = {"now": demo.timestamp("2026-10-07T00:00:00Z")}
        observed_at = demo.dt.datetime.fromtimestamp(clock["now"], demo.dt.timezone.utc).isoformat()
        c["alert_receipts_since"] = "2026-10-06T23:59:00Z"
        class DueSubset(FakeLive):
            failure = None
            _audit_events = demo.Live._audit_events
            _renewal_outcomes = demo.Live._renewal_outcomes
            renewal_outcomes = demo.Live.renewal_outcomes
            audit = demo.Live.audit
            metric_samples = demo.Live.metric_samples
            query = demo.Live.query
            reads = 0
            def cluster(self):
                self.reads += 1
                if scenario == "scheduled-renewal" and self.reads >= 2:
                    self.started = True
                    self.owner = "renew-k8s-certs-" + c["cluster_id"] + "-1234567890"
                result = super().cluster()
                result["conditions"]["certificateRenewal"].update(
                    forced=scenario != "scheduled-renewal", scheduledFor=observed_at, outcome=owner_outcome)
                return result
            def certificates(self, after=0, manual=False):
                before, renewed = snapshot(c), snapshot(c, True)
                # One node naturally has due leaves, while two are outside the threshold.
                for key in before:
                    if key[0] == c["nodes"][0]["id"] and key[1] in demo.LEAF:
                        before[key]["not_after"] = "2026-10-10T00:00:00Z"
                if self.started:
                    ids = {c["nodes"][i]["id"] for i in changed_nodes}
                    before = {key: renewed[key] if key[0] in ids else value for key, value in before.items()}
                    if certificate_change:
                        certificate_change(before, c)
                return before
            def health(self):
                return {"members": ["1", "2", "3"], "components": {
                    (n["name"], component): "after" if self.started and i in restarted_nodes else "before"
                    for i, n in enumerate(c["nodes"]) for component in demo.COMPONENTS}}
            def sql(self, query):
                # Exercise the actual audit loader and validator, not a fabricated outcome map.
                assert "credential_audit_events" in query
                assert c["cluster_id"] in query and demo.audit_hash("workflow", self.owner) in query
                events = []
                for i, n in enumerate(c["nodes"]):
                    for action in ("check", "renew", "verify"):
                        noop = action == "renew" and i != 0
                        events.append({"schema_version": "nke.credential-audit.v1",
                            "event_id": demo.digest(n["id"] + action), "timestamp": observed_at,
                            "plane": "t2", "credential_type": "kubeadm_leaf_bundle", "action": action,
                            "outcome": "no_op" if noop else "success",
                            "reason_code": "verified" if action in {"check", "verify"} else ("unchanged" if noop else "completed"),
                            "target": {"cluster_id": c["cluster_id"], "node_id": n["id"]},
                            "actor": {"kind": "automatic"},
                            "correlation": {"workflow_hash": demo.audit_hash("workflow", self.owner),
                                "operation_hash": demo.audit_hash("operation", "k8s-cert-renewal-" + demo.digest(self.owner + "/" + n["id"]))
                                    if action == "renew" else demo.audit_hash("operation", c["cluster_id"]),
                                "run_hash": demo.audit_hash("run", "fixture-run"),
                                "activity_hash": demo.audit_hash("activity", n["id"] + action), "attempt": 1}})
                events.append({"schema_version": "nke.credential-audit.v1", "event_id": demo.digest("complete"),
                    "timestamp": observed_at, "plane": "t2", "credential_type": "kubeadm_leaf_bundle",
                    "action": "renew", "outcome": "success", "reason_code": "completed",
                    "target": {"cluster_id": c["cluster_id"]}, "actor": {"kind": "automatic"},
                    "correlation": {"workflow_hash": demo.audit_hash("workflow", self.owner),
                        "operation_hash": demo.audit_hash("operation", self.owner),
                        "run_hash": demo.audit_hash("run", "fixture-run"),
                        "activity_hash": demo.audit_hash("activity", "complete"), "attempt": 1}})
                return audit_change(events, c) if audit_change else events
            def metrics(self, certificates, after=0):
                self.prometheus = SampledPrometheus(c)
                self.prometheus.scrape(certificates, clock["now"], clock["now"], clock["now"])
                demo.Live.metrics(self, certificates, after)
            def http(self, *args):
                return self.prometheus.http(*args)
        def instant(check, _deadline):
            return check()
        with patch.object(demo, "Traffic", FakeTraffic), patch.object(demo, "wait_for", side_effect=instant), \
                patch.object(demo.time, "time", side_effect=lambda: clock["now"]):
            return demo.run(c, scenario, c["cluster_id"], DueSubset)

    def test_one_due_node_and_two_healthy_unchanged_nodes_pass_whole_scheduled_lane(self):
        report = self.run_scheduled()
        self.assertEqual("passed", report["status"], report.get("reason"))
        self.assertEqual(demo.required_checks("scheduled-renewal"), set(report["checks"]))
        self.assertIn("scheduled_per_node_renewal_outcomes_observed", report["checks"])
        self.assertIn("renewed_static_pod_components_replaced", report["checks"])
        self.assertIn("fresh_metrics_after_mutation", report["checks"])
        self.assertIn("required_audit_actions_observed", report["checks"])
        self.assertEqual(["renewed", "unchanged", "unchanged"], [n["outcome"] for n in report["renewal_outcomes"]])
        self.assertEqual(2, sum(e["action"] == "renew" and e["outcome"] == "no_op" for e in report["audit_events"]))
        before = {(v["node_id"], v["credential"]): v for v in report["certificates_before"]}
        after = {(v["node_id"], v["credential"]): v for v in report["certificates_after"]}
        unchanged = {n["node_id"] for n in report["renewal_outcomes"] if n["outcome"] == "unchanged"}
        self.assertTrue(all(after[key] == value for key, value in before.items() if key[0] in unchanged))

    def test_all_noop_cannot_prove_renewal_even_with_false_overall_renewed_state(self):
        def all_noop(events, _c):
            for event in events:
                if event["action"] == "renew":
                    event.update(outcome="no_op", reason_code="unchanged")
            return events
        for owner in ("unchanged", "renewed"):
            with self.subTest(owner=owner):
                report = self.run_scheduled((), (), all_noop, owner)
                self.assertEqual("failed", report["status"])
                self.assertEqual("scheduled_cycle_did_not_renew", report["reason"])

    def test_claimed_renewal_requires_changed_fingerprint_and_later_expiry_on_that_node(self):
        report = self.run_scheduled((), (0,))
        self.assertEqual("leaf_certificate_not_renewed", report["reason"])
        def unchanged_expiry(certificates, c):
            certificates[(c["nodes"][0]["id"], "apiserver")]["not_after"] = "2026-10-10T00:00:00Z"
        report = self.run_scheduled(certificate_change=unchanged_expiry)
        self.assertEqual("leaf_certificate_not_renewed", report["reason"])

    def test_noop_leaves_must_remain_unchanged_and_all_cas_must_remain_unchanged(self):
        report = self.run_scheduled((0, 1), (0,))
        self.assertEqual("unexpected_leaf_certificate_change", report["reason"])
        def changed_ca(certificates, c):
            certificates[(c["nodes"][1]["id"], "kubernetes-ca")]["sha256"] = demo.digest("different-ca")
        report = self.run_scheduled(certificate_change=changed_ca)
        self.assertEqual("ca_certificate_changed", report["reason"])

    def test_components_change_only_on_renewed_nodes(self):
        for restarted, reason in (((), "renewed_components_not_replaced"),
                                  ((0, 1), "unchanged_components_replaced")):
            with self.subTest(restarted=restarted):
                report = self.run_scheduled(restarted_nodes=restarted)
                self.assertEqual("failed", report["status"])
                self.assertEqual(reason, report["reason"])

    def test_missing_noop_and_postflight_audit_cannot_pass(self):
        for action in ("renew", "verify"):
            with self.subTest(action=action):
                def missing(events, c):
                    return [e for e in events if not (e["target"].get("node_id") == c["nodes"][1]["id"] and e["action"] == action)]
                report = self.run_scheduled(audit_change=missing)
                self.assertEqual("failed", report["status"])
                self.assertEqual("per_node_audit_coverage_incomplete", report["reason"])

    def test_contradictory_audit_outcomes_or_reasons_fail(self):
        for contradiction in ("outcome", "reason"):
            with self.subTest(contradiction=contradiction):
                def conflicting(events, c):
                    event = next(e for e in events if e["target"].get("node_id") == c["nodes"][1]["id"] and e["action"] == "renew")
                    if contradiction == "outcome":
                        events.append({**event, "event_id": demo.digest("contradiction"), "outcome": "success", "reason_code": "completed"})
                    else:
                        event["reason_code"] = "completed"
                    return events
                report = self.run_scheduled(audit_change=conflicting)
                self.assertEqual("renewal_audit_outcome_conflict", report["reason"])

    def test_cluster_summary_never_supplies_missing_per_node_proof_and_must_agree(self):
        def summary_only(events, _c):
            return [e for e in events if e["action"] != "renew" or "node_id" not in e["target"]]
        report = self.run_scheduled(audit_change=summary_only)
        self.assertEqual("per_node_audit_coverage_incomplete", report["reason"])
        def wrong_summary(events, _c):
            events[-1].update(outcome="no_op", reason_code="unchanged")
            return events
        report = self.run_scheduled(audit_change=wrong_summary)
        self.assertEqual("renewal_audit_outcome_conflict", report["reason"])

    def test_renewal_outcome_for_unknown_node_cannot_qualify(self):
        def wrong_node(events, _c):
            event = next(e for e in events if e["action"] == "renew" and "node_id" in e["target"])
            event["target"]["node_id"] = "00000000-0000-0000-0000-000000009999"
            return events
        report = self.run_scheduled(audit_change=wrong_node)
        self.assertEqual("renewal_audit_node_inventory_mismatch", report["reason"])

    def test_failed_legacy_attempt_then_successful_retry_qualifies_partial_cycle(self):
        for index in (0, 1):
            with self.subTest(final="renewed" if index == 0 else "unchanged"):
                def retried(events, c):
                    success = next(e for e in events if e["action"] == "renew" and e["target"].get("node_id") == c["nodes"][index]["id"])
                    failure = copy.deepcopy(success)
                    failure.update(event_id=demo.digest("first-failed-attempt"), outcome="failure", reason_code="operation_failed")
                    success["correlation"]["attempt"] = 2
                    # SQL aggregation need not order observations chronologically.
                    return events + [failure]
                report = self.run_scheduled(audit_change=retried)
                self.assertEqual("passed", report["status"], report.get("reason"))
                self.assertTrue(any(e["action"] == "renew" and e["outcome"] == "failure" for e in report["audit_events"]))
                self.assertIn("fresh_metrics_after_mutation", report["checks"])

    def test_later_failure_or_failure_in_another_stage_is_not_hidden_by_success(self):
        for change in ("later_attempt", "another_activity", "another_run"):
            with self.subTest(change=change):
                def conflicting(events, c):
                    success = next(e for e in events if e["action"] == "renew" and e["target"].get("node_id") == c["nodes"][0]["id"])
                    failure = copy.deepcopy(success)
                    failure.update(event_id=demo.digest("unresolved-failure"), outcome="failure", reason_code="operation_failed")
                    if change == "later_attempt":
                        failure["correlation"]["attempt"] = 2
                    else:
                        field = "activity_hash" if change == "another_activity" else "run_hash"
                        failure["correlation"][field] = demo.digest("different-stage")
                        success["correlation"]["attempt"] = 2
                    return events + [failure]
                report = self.run_scheduled(audit_change=conflicting)
                self.assertEqual("renewal_audit_outcome_conflict", report["reason"])

    def test_missing_or_wrong_operation_and_attempt_correlation_cannot_order_retries(self):
        for field, value in (("operation_hash", demo.digest("unrelated-operation")), ("attempt", 0),
                             ("attempt", True), ("activity_hash", ""), ("run_hash", "")):
            with self.subTest(field=field, value=value):
                def wrong(events, c):
                    event = next(e for e in events if e["action"] == "renew" and e["target"].get("node_id") == c["nodes"][0]["id"])
                    event["correlation"][field] = value
                    return events
                report = self.run_scheduled(audit_change=wrong)
                self.assertEqual("renewal_audit_correlation_invalid", report["reason"])

    def test_forced_and_alert_still_require_all_control_plane_leaves_and_components(self):
        for scenario in ("forced-renewal", "alert-renewal"):
            with self.subTest(scenario=scenario):
                with patch.object(FakeLive, "alert", return_value=True, create=True):
                    report = self.run_scheduled(scenario=scenario)
                self.assertEqual("leaf_certificate_not_renewed", report["reason"])
                with patch.object(FakeLive, "alert", return_value=True, create=True):
                    report = self.run_scheduled((0, 1, 2), (0,), scenario=scenario)
                self.assertEqual("renewed_components_not_replaced", report["reason"])


class CollectorContractTests(unittest.TestCase):
    def test_missing_never_used_failure_counter_is_healthy_only_with_stored_evidence(self):
        c = config()
        live = demo.Live(c)
        certificates = snapshot(c)
        prometheus = SampledPrometheus(c)
        sampled = time.time() - 1
        prometheus.scrape(certificates, sampled, sampled - 1, sampled - 1)
        with patch.object(live, "http", side_effect=prometheus.http):
            live.metrics(certificates)
            prometheus.stored = 0
            with self.assertRaisesRegex(demo.CheckError, "audit_sink_unhealthy_or_unobserved"):
                live.metrics(certificates)

    def test_preceding_scrape_and_fresh_wrong_expiry_do_not_prove_renewal(self):
        c = config()
        before, after = snapshot(c), snapshot(c, True)
        live, prometheus = demo.Live(c), SampledPrometheus(c)
        epoch = demo.timestamp("2026-10-07T00:00:00Z")
        prometheus.scrape(before, epoch, epoch - 1, epoch - 1)
        selector = 'nvke_controller_certificate_inspection_available{tenant_cluster=' + json.dumps(c["cluster_name"]) + '}'
        with patch.object(live, "http", side_effect=prometheus.http), patch.object(demo.time, "time", return_value=epoch + 50):
            # Prometheus evaluates now but serves the preceding scrape's value.
            self.assertEqual(epoch + 50, live.query(selector)[0]["value"][0])
            self.assertEqual(str(epoch), live.query("timestamp(" + selector + ")")[0]["value"][1])
            live.metrics(before)
            with self.assertRaisesRegex(demo.CheckError, "metric_sample_stale"):
                live.metrics(after, epoch + 10)
            with self.assertRaisesRegex(demo.CheckError, "certificate_metric_snapshot_mismatch"):
                live.metrics(after)
        # Even a scrape after completion must contain the independently observed expiry.
        prometheus.scrape(before, epoch + 60, epoch + 55, epoch + 55)
        with patch.object(live, "http", side_effect=prometheus.http), patch.object(demo.time, "time", return_value=epoch + 61):
            with self.assertRaisesRegex(demo.CheckError, "certificate_metric_snapshot_mismatch"):
                live.metrics(after, epoch + 10)

    def test_csr_age_290_plus_50_seconds_of_lookback_is_stale(self):
        c = config()
        certificates = snapshot(c)
        live, prometheus = demo.Live(c), SampledPrometheus(c)
        sampled = demo.timestamp("2026-10-07T00:00:00Z")
        prometheus.scrape(certificates, sampled, sampled - 1, sampled - 290)
        with patch.object(live, "http", side_effect=prometheus.http), patch.object(demo.time, "time", return_value=sampled + 50):
            with self.assertRaisesRegex(demo.CheckError, "csr_metric_stale"):
                live.metrics(certificates)

    def test_baseline_and_completed_metrics_support_every_t2_scenario(self):
        c = config()
        epoch = demo.timestamp("2026-10-07T00:00:00Z")
        for scenario in set(demo.SCENARIOS) - {"manual-t1"}:
            with self.subTest(scenario=scenario):
                c["scenario"] = scenario
                before = snapshot(c)
                # Data-key rotation preserves certificate identities and expiry.
                after = snapshot(c, scenario != "etcd-key")
                live, prometheus = demo.Live(c), SampledPrometheus(c)
                prometheus.scrape(before, epoch - 1, epoch - 2, epoch - 2)
                with patch.object(live, "http", side_effect=prometheus.http), patch.object(demo.time, "time", return_value=epoch):
                    live.metrics(before)
                prometheus.scrape(after, epoch + 2, epoch + 1, epoch + 1)
                with patch.object(live, "http", side_effect=prometheus.http), patch.object(demo.time, "time", return_value=epoch + 3):
                    live.metrics(after, epoch)

    def test_post_completion_scrape_requires_post_completion_observations(self):
        c = config()
        certificates = snapshot(c)
        completion = demo.timestamp("2026-10-07T00:00:00Z")
        for inspected, csr_observed, reason in ((completion - 1, completion + 1, "inspection_metric_stale"),
                                                (completion + 1, completion - 1, "csr_metric_stale")):
            with self.subTest(reason=reason):
                live, prometheus = demo.Live(c), SampledPrometheus(c)
                prometheus.scrape(certificates, completion + 5, inspected, csr_observed)
                with patch.object(live, "http", side_effect=prometheus.http), patch.object(demo.time, "time", return_value=completion + 10):
                    with self.assertRaisesRegex(demo.CheckError, reason):
                        live.metrics(certificates, completion)

    def test_final_stage_waits_for_matching_fresh_sample_with_original_deadline(self):
        c = config()
        epoch = demo.timestamp("2026-10-07T00:00:00Z")
        clock = {"now": epoch, "sleeps": 0}
        prometheus = SampledPrometheus(c)
        prometheus.scrape(snapshot(c), epoch - 1, epoch - 2, epoch - 2)
        class MetricsLive(FakeLive):
            failure = None
            metrics = demo.Live.metrics
            metric_samples = demo.Live.metric_samples
            query = demo.Live.query
            def http(self, *args):
                return prometheus.http(*args)
        def scrape_after_wait(seconds):
            clock["sleeps"] += 1
            clock["now"] += seconds
            prometheus.scrape(snapshot(c, True), clock["now"] - 0.5, epoch + 0.5, epoch + 0.5)
        with patch.object(demo, "Traffic", FakeTraffic), \
                patch.object(demo, "command", side_effect=lambda argv: c["deployment_source_revision"].encode() if argv[-1] == "HEAD" else b""), \
                patch.object(demo.time, "time", side_effect=lambda: clock["now"]), \
                patch.object(demo.time, "monotonic", side_effect=lambda: clock["now"] - epoch), \
                patch.object(demo.time, "sleep", side_effect=scrape_after_wait):
            result = demo.run(c, "forced-renewal", c["cluster_id"], MetricsLive)
        self.assertEqual("passed", result["status"], result.get("reason"))
        self.assertEqual(1, clock["sleeps"])
        self.assertIn("fresh_metrics_after_mutation", result["checks"])
        certificate_queries = [evaluated for expression, evaluated in prometheus.queries if expression.startswith("label_replace(")]
        self.assertEqual({epoch, epoch + 2}, set(certificate_queries))

    def test_final_metric_wait_expires_without_extending_original_deadline(self):
        c = config()
        epoch = demo.timestamp("2026-10-07T00:00:00Z")
        clock = {"now": epoch, "sleeps": 0}
        prometheus = SampledPrometheus(c)
        prometheus.scrape(snapshot(c), epoch - 1, epoch - 2, epoch - 2)
        class MetricsLive(FakeLive):
            failure = None
            metrics = demo.Live.metrics
            metric_samples = demo.Live.metric_samples
            query = demo.Live.query
            def http(self, *args):
                return prometheus.http(*args)
        def advance(seconds):
            clock["sleeps"] += 1
            clock["now"] += seconds
        with patch.object(demo, "Traffic", FakeTraffic), \
                patch.object(demo, "command", side_effect=lambda argv: c["deployment_source_revision"].encode() if argv[-1] == "HEAD" else b""), \
                patch.object(demo.time, "time", side_effect=lambda: clock["now"]), \
                patch.object(demo.time, "monotonic", side_effect=lambda: clock["now"] - epoch), \
                patch.object(demo.time, "sleep", side_effect=advance):
            result = demo.run(c, "forced-renewal", c["cluster_id"], MetricsLive)
        self.assertEqual("failed", result["status"])
        self.assertEqual("metric_sample_stale", result["reason"])
        self.assertNotIn("fresh_metrics_after_mutation", result["checks"])
        self.assertEqual(epoch + c["timeout_seconds"], clock["now"])
        self.assertEqual(5, clock["sleeps"])

    def test_nonfinite_duplicate_and_conflicting_replica_metrics_fail(self):
        c = config()
        certificates = snapshot(c)
        sampled = demo.timestamp("2026-10-07T00:00:00Z")
        for invalid in ("NaN", "+Inf", "-Inf"):
            with self.subTest(value=invalid):
                live, prometheus = demo.Live(c), SampledPrometheus(c)
                prometheus.scrape(certificates, sampled, sampled - 1, sampled - 1)
                prometheus.put("serving_csr_observation_age_seconds", {"instance": "controller-1"}, sampled + 1, invalid)
                with patch.object(live, "http", side_effect=prometheus.http), patch.object(demo.time, "time", return_value=sampled + 2):
                    with self.assertRaisesRegex(demo.CheckError, "metric_value_invalid"):
                        live.metrics(certificates)
        live, prometheus = demo.Live(c), SampledPrometheus(c)
        prometheus.scrape(certificates, sampled, sampled - 1, sampled - 1)
        def duplicate(*args):
            response = prometheus.http(*args)
            if "result" in response["data"] and response["data"]["result"]:
                response["data"]["result"].append(copy.deepcopy(response["data"]["result"][0]))
            return response
        with patch.object(live, "http", side_effect=duplicate), patch.object(demo.time, "time", return_value=sampled + 2):
            with self.assertRaisesRegex(demo.CheckError, "metric_series_duplicate"):
                live.metrics(certificates)
        # Multiple scraped replicas are supported only when every replica matches.
        prometheus.scrape(certificates, sampled, sampled - 1, sampled - 1, "controller-2")
        with patch.object(live, "http", side_effect=prometheus.http), patch.object(demo.time, "time", return_value=sampled + 2):
            live.metrics(certificates)
            prometheus.put("certificate_not_after_timestamp_seconds", {"node": c["nodes"][0]["name"], "credential": "apiserver", "instance": "controller-2"}, sampled + 1, 1)
            with self.assertRaisesRegex(demo.CheckError, "certificate_metric_snapshot_mismatch"):
                live.metrics(certificates)

    def serving_certificates(self, csrs, served=None):
        c = config()
        live, certificates = demo.Live(c), snapshot(c)
        der = {n["kubelet_address"]: b"der-" + n["name"].encode() for n in c["nodes"]}
        for n in c["nodes"]:
            certificates[(n["id"], "kubelet-serving")]["sha256"] = demo.hashlib.sha256(der[n["kubelet_address"]]).hexdigest()
        class Connection:
            def __init__(self, address=None):
                self.address = address
            def __enter__(self):
                return self
            def __exit__(self, *_args):
                return False
            def wrap_socket(self, sock, server_hostname):
                return Connection(server_hostname)
            def getpeercert(self, binary_form):
                return served or der[self.address]
        csr_items = [{"spec": {"signerName": "kubernetes.io/kubelet-serving", "username": "system:node:" + n["name"]},
                      "status": status} for n, status in csrs(c)]
        with patch.object(live, "kubejson", return_value={"items": csr_items}), \
                patch.object(demo.ssl, "create_default_context", return_value=Connection()), \
                patch.object(demo.socket, "create_connection", return_value=Connection()), \
                patch.object(demo, "command", side_effect=lambda argv, stdin=None: stdin):
            return live.serving_certificates(certificates)

    def test_installed_and_served_certificate_qualifies_after_its_csr_was_garbage_collected(self):
        result = self.serving_certificates(lambda _c: [])
        self.assertEqual([False] * 3, [x["csr_observed"] for x in result])
        self.assertTrue(all(x["installed_served"] for x in result))

    def test_still_listed_issued_csr_is_recorded_as_optional_evidence(self):
        def listed(c):
            return [(n, {"certificate": base64.b64encode(b"der-" + n["name"].encode()).decode(),
                         "conditions": [{"type": "Approved", "status": "True"}]}) for n in c["nodes"]]
        self.assertEqual([True] * 3, [x["csr_observed"] for x in self.serving_certificates(listed)])

    def test_unrelated_old_pending_or_denied_csr_neither_qualifies_nor_blocks(self):
        def unrelated(c):
            return [(c["nodes"][0], {}), (c["nodes"][1], {"conditions": [{"type": "Denied", "status": "True"}]})]
        self.assertEqual([False] * 3, [x["csr_observed"] for x in self.serving_certificates(unrelated)])

    def test_certificate_not_served_by_the_kubelet_fails_even_with_a_matching_csr(self):
        def listed(c):
            return [(n, {"certificate": base64.b64encode(b"der-" + n["name"].encode()).decode(),
                         "conditions": [{"type": "Approved", "status": "True"}]}) for n in c["nodes"]]
        with self.assertRaisesRegex(demo.CheckError, "installed_certificate_not_served"):
            self.serving_certificates(listed, served=b"another-certificate")

    def test_authorization_header_is_never_sent_to_a_redirect_target(self):
        seen = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append((self.server.server_port, self.path, self.headers.get("Authorization")))
                if self.path == "/start":
                    self.send_response(302)
                    self.send_header("Location", "http://127.0.0.1:%d/leak" % self.server.other)
                    self.end_headers()
                    return
                body = b"{}"
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            def log_message(self, *_args):
                pass
        servers = [http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler) for _ in range(2)]
        servers[0].other, servers[1].other = servers[1].server_port, servers[0].server_port
        threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in servers]
        for thread in threads:
            thread.start()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                c = config()
                token = Path(tmp) / "token"
                token.write_text("Bearer REDIRECT_SENTINEL\n")
                c["cwe_authorization_file"] = str(token)
                live = demo.Live(c)
                with patch.dict(os.environ, {"NO_PROXY": "127.0.0.1", "no_proxy": "127.0.0.1"}):
                    self.assertEqual({}, live.http("http://127.0.0.1:%d" % servers[0].server_port, "/ok", "cwe_authorization_file"))
                    with self.assertRaisesRegex(demo.CheckError, "service_unavailable"):
                        live.http("http://127.0.0.1:%d" % servers[0].server_port, "/start", "cwe_authorization_file")
        finally:
            for server in servers:
                server.shutdown()
                server.server_close()
        self.assertEqual([(servers[0].server_port, "/ok", "Bearer REDIRECT_SENTINEL"),
                          (servers[0].server_port, "/start", "Bearer REDIRECT_SENTINEL")], seen)

    def test_services_receiving_the_token_require_https(self):
        for key in ("cwe_url", "prometheus_url"):
            with self.subTest(key=key):
                c = config()
                c[key] = "http://" + c[key].removeprefix("https://")
                with self.assertRaisesRegex(demo.CheckError, "insecure_service_url"):
                    demo.validate_config(c, "forced-renewal")

    def test_go_fractional_seconds_of_any_length_parse(self):
        base = demo.timestamp("2026-10-07T00:00:00Z")
        for fraction in ("1", "12", "123", "123456", "1234567", "123456789"):
            for zone in ("Z", "+00:00"):
                with self.subTest(fraction=fraction, zone=zone):
                    self.assertAlmostEqual(base + float("0." + fraction), demo.timestamp("2026-10-07T00:00:00." + fraction + zone), places=5)
        self.assertEqual(base - 7200, demo.timestamp("2026-10-07T00:00:00.123456789+02:00") - 0.123456)

    def test_manual_public_inventory_excludes_kubelet_and_worker_credentials(self):
        c = config()
        live = demo.Live(c)
        public = [{"credential": name, "state": "available", "certificateSHA256": demo.digest(name),
                   "notAfter": "2028-01-01T00:00:00Z"} for name in demo.CA | demo.LEAF]
        observation = json.dumps({"available": True, "observedAt": demo.now(), "certificates": public}).encode()
        with patch.object(demo, "command", return_value=observation):
            result = live.certificates(manual=True)
        self.assertEqual(39, len(result))
        self.assertFalse(any(name.startswith("kubelet") for _node, name in result))

    def test_manual_audit_is_bound_to_exact_target_run_and_per_node_actions(self):
        c = config()
        c["scenario"] = "manual-t1"
        events = []
        for n in c["nodes"]:
            for action in ("check", "renew", "verify"):
                events.append({"schema_version": "nke.credential-audit.v1", "event_id": demo.digest(n["name"] + action),
                    "timestamp": demo.now(), "plane": "t1", "credential_type": "kubeadm_leaf_bundle", "action": action,
                    "outcome": "success", "target": {"cluster_hash": demo.audit_hash("cluster", c["kube_system_uid"]),
                        "node_hash": demo.audit_hash("node", n["name"])}, "actor": {"kind": "operator_reported"},
                    "correlation": {"operation_hash": demo.audit_hash("operation", c["manual_operation"])}})
        with tempfile.TemporaryDirectory() as tmp:
            c["manual_audit_file"] = str(Path(tmp) / "audit.jsonl")
            def save():
                Path(c["manual_audit_file"]).write_text("\n".join(json.dumps(e) for e in events))
            save()
            self.assertEqual(9, len(demo.Live(c).audit("unused", time.time() - 10, True)))
            events[0]["correlation"]["operation_hash"] = "a" * 64
            save()
            with self.assertRaisesRegex(demo.CheckError, "target_or_run_mismatch"):
                demo.Live(c).audit("unused", time.time() - 10, True)

    def test_manual_audit_accepts_per_step_identities_and_rejects_duplicate_events(self):
        c = config()
        c["scenario"] = "manual-t1"
        def event(node, action, operation):
            identity = {"plane": "t1", "credential_type": "kubeadm_leaf_bundle", "action": action, "outcome": "success",
                        "target": {"cluster_hash": demo.audit_hash("cluster", c["kube_system_uid"]),
                                   "node_hash": demo.audit_hash("node", node)},
                        "actor": {"kind": "operator_reported"}, "correlation": {"operation_hash": demo.audit_hash("operation", operation)}}
            return {"schema_version": "nke.credential-audit.v1", "event_id": demo.digest(json.dumps(identity, sort_keys=True)),
                    "timestamp": demo.now(), **identity}
        run = c["manual_operation"]
        events = []
        for n in c["nodes"]:
            events += [event(n["name"], "check", run + "/check-1"), event(n["name"], "renew", run + "/renew-1"),
                       event(n["name"], "verify", run + "/verify-1")]
        # A failed renew repeated identically on one node keeps a distinct row through its step ID.
        events.append(event(c["nodes"][0]["name"], "renew", run + "/renew-2"))
        with tempfile.TemporaryDirectory() as tmp:
            c["manual_audit_file"] = str(Path(tmp) / "audit.jsonl")
            def save():
                Path(c["manual_audit_file"]).write_text("\n".join(json.dumps(e) for e in events))
            save()
            self.assertEqual(10, len(demo.Live(c).audit("unused", time.time() - 10, True)))
            events.append(event(c["nodes"][0]["name"], "renew", run + "/renew-2"))
            save()
            with self.assertRaisesRegex(demo.CheckError, "audit_event_duplicate"):
                demo.Live(c).audit("unused", time.time() - 10, True)
            events[-1] = event(c["nodes"][0]["name"], "renew", "another-run/renew-3")
            save()
            with self.assertRaisesRegex(demo.CheckError, "target_or_run_mismatch"):
                demo.Live(c).audit("unused", time.time() - 10, True)

    def test_workflow_type_and_terminal_state_are_checked(self):
        live = demo.Live(config())
        info = {"workflowExecutionInfo": {"execution": {"workflowId": "expected"}, "type": {"name": "RenewK8sCertsV2"}, "status": "WORKFLOW_EXECUTION_STATUS_COMPLETED"}}
        with patch.object(live, "temporal", return_value=info):
            self.assertEqual("COMPLETED", live.workflow("expected", "RenewK8sCertsV2"))
            with self.assertRaisesRegex(demo.CheckError, "unsupported_workflow"):
                live.workflow("expected", "RotateEtcdEncryptionKey")
            info["workflowExecutionInfo"]["status"] = "FAILED"
            with self.assertRaisesRegex(demo.CheckError, "workflow_failed"):
                live.workflow("expected", "RenewK8sCertsV2")

    def test_deployed_controller_and_agent_versions_must_match_expectations(self):
        c = config()
        live = demo.Live(c)
        nodes = {"items": [{"metadata": {"name": n["name"]}, "status": {"nodeInfo": {"kubeletVersion": "v1.35.1"}}} for n in c["nodes"]]}
        agents = [{"id": n["id"], "version": "example-agent-version"} for n in c["nodes"]]
        with patch.object(live, "kubejson", return_value=nodes), patch.object(live, "sql", return_value=agents), \
                patch.object(live, "http", return_value={"version": "example-build-version", "temporalUIURL": "SECRET_SENTINEL"}):
            report = live.versions()
            self.assertNotIn("SECRET_SENTINEL", json.dumps(report))
            self.assertNotIn("example-agent-version", json.dumps(report))
            agents[0]["version"] = "unrelated-deployment"
            with self.assertRaisesRegex(demo.CheckError, "deployed_version_mismatch"):
                live.versions()


class ProofTests(unittest.TestCase):
    def live(self, old=False, stale=False, bad_payload=False, plaintext=False):
        c = config()
        class ProofLive:
            def __init__(self):
                self.c = c
            def kubejson(self, *args):
                if "secret" in args:
                    return {"data": {"probe": base64.b64encode(b"changed" if bad_payload else b"nvke-rotation-public-fixture").decode()}}
                return {"items": [{"metadata": {"name": "public-pod"}, "spec": {"nodeName": n["name"]}, "status": {"podIP": n["kubelet_address"]}} for n in c["nodes"]]}
            def kubectl(self, *args):
                if "--print-value-only" in args:
                    name = args[-2].rsplit("/", 1)[-1]
                    key = "old" if old or (stale and name.endswith("-1")) else "new"
                    return b"nvke-rotation-public-fixture" if plaintext else ("k8s:enc:secretbox:v1:" + key + ":").encode() + b"\0CIPHERTEXT_SENTINEL"
                return b""
        return ProofLive()

    def test_old_secret_rewrite_every_api_primary_and_payload(self):
        previous = demo.digest("k8s:enc:secretbox:v1:old")
        self.assertEqual(demo.digest("k8s:enc:secretbox:v1:new"), demo.EtcdProof(self.live()).verify(previous))
        for kw, reason in (({"old": True}, "not_rewritten"), ({"stale": True}, "not_converged"),
                           ({"bad_payload": True}, "payload_changed"), ({"plaintext": True}, "plaintext")):
            with self.subTest(kw=kw), self.assertRaisesRegex(demo.CheckError, reason):
                demo.EtcdProof(self.live(**kw)).verify(previous)


if __name__ == "__main__":
    unittest.main()
