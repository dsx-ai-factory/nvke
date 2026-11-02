# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Live credential qualification. Command output stays private; evidence is allowlisted."""
import argparse
import base64
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import re
import ssl
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import uuid

SCENARIOS = ("forced-renewal", "scheduled-renewal", "alert-renewal", "etcd-key", "manual-t1")
CA = {"kubernetes-ca", "front-proxy-ca", "etcd-ca"}
LEAF = {"apiserver", "apiserver-kubelet-client", "front-proxy-client", "apiserver-etcd-client",
        "etcd-server", "etcd-peer", "etcd-healthcheck-client", "admin", "controller-manager", "scheduler"}
CATALOG = CA | LEAF | {"super-admin", "kubelet-client", "kubelet-serving"}
COMPONENTS = {"kube-apiserver", "etcd", "kube-controller-manager", "kube-scheduler"}
HEX = re.compile(r"[a-f0-9]{64}\Z")
MAX_BYTES = 8 * 1024 * 1024
SOURCE_ROOT = Path(__file__).resolve().parents[2]


class CheckError(Exception):
    """Only fixed reason codes may escape the collector."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """The Authorization header must never follow a redirect to another host or scheme."""
    def redirect_request(self, *_args, **_kwargs):
        return None


def require(condition, reason):
    if not condition:
        raise CheckError(reason)


def digest(value):
    return hashlib.sha256(value.encode()).hexdigest()


def audit_hash(domain, value):
    return digest(domain + "\0" + value)


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def timestamp(value):
    # Go writes 1-9 fractional digits; Python 3.10 fromisoformat accepts exactly 3 or 6.
    value = re.sub(r"\.(\d+)", lambda m: "." + m.group(1)[:6].ljust(6, "0"), value.replace("Z", "+00:00"), count=1)
    return dt.datetime.fromisoformat(value).timestamp()


def metric_number(value):
    try:
        number = float(value)
    except (TypeError, ValueError, OverflowError):
        raise CheckError("metric_value_invalid") from None
    require(math.isfinite(number), "metric_value_invalid")
    return number


def read_json(path):
    require(Path(path).stat().st_size <= MAX_BYTES, "input_too_large")
    return json.loads(Path(path).read_text())


def write_json(path, value):
    # Exclusive creation prevents following/replacing existing files, including symlinks.
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as out:
        json.dump(value, out, indent=2, sort_keys=True)
        out.write("\n")


def command(argv, stdin=None, timeout=30):
    # Tool diagnostics can contain tokens, kubeconfigs, object bodies or Temporal payloads.
    # Capture privately and never put them in exceptions, logs or evidence.
    with tempfile.TemporaryFile() as stdout, tempfile.TemporaryFile() as stderr:
        try:
            result = subprocess.run(argv, input=stdin, stdout=stdout, stderr=stderr, timeout=timeout,
                                    check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise CheckError("command_unavailable_or_timed_out") from None
        require(result.returncode == 0, "command_failed")
        require(stdout.tell() <= MAX_BYTES, "command_output_too_large")
        stdout.seek(0)
        return stdout.read()


def validate_config(c, scenario):
    require(scenario in SCENARIOS, "invalid_scenario")
    require(str(uuid.UUID(c["cluster_id"])) == c["cluster_id"], "invalid_cluster_id")
    require(str(uuid.UUID(c["kube_system_uid"])) == c["kube_system_uid"], "invalid_target_uid")
    require(c["profile"] in {"kubeadm-ha", "kubeadm-single"}, "unsupported_profile")
    require(bool(c["context"]) and bool(c["nodes"]), "missing_target")
    require(c.get("api_server", "").startswith("https://"), "missing_api_endpoint_identity")
    cp = [n for n in c["nodes"] if n["control_plane"]]
    require(len(cp) == (3 if c["profile"] == "kubeadm-ha" else 1), "unsupported_topology")
    require(len({n["name"] for n in c["nodes"]}) == len(c["nodes"]), "duplicate_node")
    require(len({n["id"] for n in c["nodes"]}) == len(c["nodes"]), "duplicate_node")
    for n in c["nodes"]:
        require(str(uuid.UUID(n["id"])) == n["id"], "invalid_node_id")
        require(bool(re.fullmatch(r"[a-z0-9][a-z0-9.-]{0,252}", n["name"])), "invalid_node_name")
        require(n["control_plane"] in (True, False), "invalid_node_role")
    require(1 <= c.get("threshold_days", 30) <= 90, "invalid_threshold")
    require(10 <= c.get("timeout_seconds", 7200) <= 28800, "invalid_timeout")
    require(1 <= c.get("traffic_interval_seconds", 2) <= 10, "invalid_traffic_interval")
    require(0 <= c.get("max_api_error_fraction", 0) <= 1, "invalid_traffic_budget")
    require(c.get("max_api_latency_seconds", 10) > 0, "invalid_traffic_budget")
    if scenario == "manual-t1":
        require(all(n.get("ssh") for n in cp), "missing_t1_ssh")
        require(c.get("manual_completion_file") and c.get("manual_audit_file"), "missing_t1_evidence_paths")
        require(c.get("manual_operation"), "missing_t1_operation_identity")
    else:
        require(c["cwe_url"] and c["prometheus_url"] and c["temporal"]["task_queue"], "missing_services")
        require(isinstance(c.get("cluster_name"), str) and bool(c["cluster_name"]), "missing_target")
        # These URLs receive the optional Authorization header.
        require(c["cwe_url"].startswith("https://") and c["prometheus_url"].startswith("https://"), "insecure_service_url")
        expected = c["expected_versions"]
        require(expected["controller"] and set(expected["agents"]) == {n["id"] for n in c["nodes"]}, "missing_deployed_version_expectations")
        # Serving-certificate proof reads these after the mutation; fail before it instead.
        require(c.get("kubelet_ca_file") and Path(c["kubelet_ca_file"]).is_file(), "missing_kubelet_ca_file")
        require(all(isinstance(n.get("kubelet_address"), str) and n["kubelet_address"] for n in c["nodes"]), "missing_kubelet_address")
    require(set(c["expected_versions"]["kubernetes"]) == {n["name"] for n in c["nodes"]}, "missing_kubernetes_version_expectations")
    require(bool(re.fullmatch(r"[a-f0-9]{40}", c["deployment_source_revision"])), "missing_deployment_revision")
    if scenario == "alert-renewal":
        require(c.get("alert_credential") in LEAF, "alert_requires_renewable_leaf")
        require(c.get("alert_node") in {n["name"] for n in cp}, "invalid_alert_node")
        require(c.get("alert_receipts_file"), "missing_alert_receiver_evidence")
    return c


MANUAL_STEP_LIMIT = 99


def manual_operation_hashes(run):
    """Operation hashes of the run ID and its per-step IDs <run>/<action>-<n>, as the audit guide suggests."""
    return {audit_hash("operation", run)} | {audit_hash("operation", run + "/" + action + "-" + str(step))
                                              for action in ("check", "approve", "renew", "rotate", "verify")
                                              for step in range(1, MANUAL_STEP_LIMIT + 1)}


class Live:
    def __init__(self, c):
        self.c = c

    def kubectl(self, *args, stdin=None):
        return command(["kubectl", "--context", self.c["context"], "--request-timeout=15s", *args], stdin)

    def kubejson(self, *args):
        return json.loads(self.kubectl(*args, "-o", "json"))

    def http(self, base, suffix, auth_key=None):
        headers = {}
        if auth_key and self.c.get(auth_key):
            # File contains the complete Authorization value, not JSON or shell code.
            token = Path(self.c[auth_key]).read_text().strip()
            require("\n" not in token and "\r" not in token, "invalid_auth_header")
            headers["Authorization"] = token
        request = urllib.request.Request(base.rstrip("/") + suffix, headers=headers)
        try:
            with urllib.request.build_opener(NoRedirect).open(request, timeout=15) as response:
                data = response.read(MAX_BYTES + 1)
        except Exception:
            raise CheckError("service_unavailable") from None
        require(len(data) <= MAX_BYTES, "service_output_too_large")
        return json.loads(data)

    def sql(self, query):
        # libpq PGSERVICE / PGPASSFILE select a read-only role; no DSN in argv or evidence.
        return json.loads(command(["psql", "-X", "-q", "-t", "-A", "-v", "ON_ERROR_STOP=1", "-c", query]))

    def cluster(self):
        value = self.http(self.c["cwe_url"], "/api/clusters/" + self.c["cluster_id"], "cwe_authorization_file")
        require(value["id"] == self.c["cluster_id"] and value["name"] == self.c["cluster_name"], "cwe_target_mismatch")
        return value

    def identity(self):
        view = self.kubejson("config", "view", "--minify")
        require(view["clusters"][0]["cluster"]["server"] == self.c["api_server"], "api_endpoint_mismatch")
        uid = self.kubejson("get", "namespace", "kube-system")["metadata"]["uid"]
        require(uid == self.c["kube_system_uid"], "kubernetes_target_mismatch")
        self.health()

    def health(self):
        require(self.kubectl("get", "--raw=/readyz").strip() == b"ok", "api_not_ready")
        nodes = self.kubejson("get", "nodes")["items"]
        require({n["metadata"]["name"] for n in nodes} == {n["name"] for n in self.c["nodes"]}, "node_inventory_changed")
        for node in nodes:
            require(any(x["type"] == "Ready" and x["status"] == "True" for x in node["status"]["conditions"]), "node_not_ready")
        pods = self.kubejson("get", "pods", "-n", "kube-system")["items"]
        expected = {n["name"] for n in self.c["nodes"] if n["control_plane"]}
        components = {}
        members = None
        for node in expected:
            for component in COMPONENTS:
                matching = [p for p in pods if p["spec"].get("nodeName") == node and
                            p["metadata"].get("labels", {}).get("component") == component]
                require(len(matching) == 1, "component_inventory_invalid")
                statuses = matching[0]["status"].get("containerStatuses", [])
                require(statuses and all(s.get("ready") for s in statuses), "component_not_ready")
                components[(node, component)] = digest("|".join(sorted(s["containerID"] for s in statuses)))
                if component == "etcd":
                    pod = matching[0]["metadata"]["name"]
                    prefix = ["exec", "-n", "kube-system", pod, "-c", "etcd", "--", "etcdctl",
                              "--endpoints=https://127.0.0.1:2379", "--cacert=/etc/kubernetes/pki/etcd/ca.crt",
                              "--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt", "--key=/etc/kubernetes/pki/etcd/healthcheck-client.key"]
                    m = json.loads(self.kubectl(*prefix, "member", "list", "--write-out=json"))["members"]
                    require({x["name"] for x in m} == expected and all(not x.get("isLearner", False) for x in m), "etcd_membership_invalid")
                    ids = {str(x["ID"]) for x in m}
                    require(members is None or members == ids, "etcd_membership_inconsistent")
                    members = ids
                    endpoints = json.loads(self.kubectl(*prefix, "endpoint", "health", "--cluster", "--write-out=json"))
                    status = json.loads(self.kubectl(*prefix, "endpoint", "status", "--cluster", "--write-out=json"))
                    require(len(endpoints) == len(expected) and all(x.get("health") is True for x in endpoints), "etcd_endpoint_unhealthy")
                    require({str(x["Status"]["header"]["member_id"]) for x in status} == members, "etcd_endpoint_missing")
                    require(all(not x["Status"].get("errors") for x in status), "etcd_endpoint_error")
        return {"members": sorted(members), "components": components}

    def versions(self, manual=False):
        expected, result = self.c["expected_versions"], []
        def observed(component, value, declared, node_id=None):
            require(isinstance(value, str) and 0 < len(value) <= 200 and value == declared, "deployed_version_mismatch")
            result.append({"component": component, "node_id": node_id, "reported_version_sha256": digest(value)})
        nodes = self.kubejson("get", "nodes")["items"]
        for n in self.c["nodes"]:
            node = next(x for x in nodes if x["metadata"]["name"] == n["name"])
            observed("kubernetes", node["status"]["nodeInfo"]["kubeletVersion"], expected["kubernetes"][n["name"]], n["id"])
        if not manual:
            controller = self.http(self.c["cwe_url"], "/api/version")["version"]
            observed("controller", controller, expected["controller"])
            agents = self.sql("SELECT COALESCE(json_agg(json_build_object('id',id,'version',agent_version)), '[]'::json) FROM nodes WHERE cluster_id='" + self.c["cluster_id"] + "'::uuid")
            require({n["id"] for n in agents} == set(expected["agents"]), "agent_version_inventory_mismatch")
            for agent in sorted(agents, key=lambda x: x["id"]):
                observed("agent", agent["version"], expected["agents"][agent["id"]], agent["id"])
        return result

    def certificates(self, after=0, manual=False):
        if manual:
            rows = []
            script = (Path(__file__).parent / "public-certificates.sh").read_bytes()
            for n in self.c["nodes"]:
                if not n["control_plane"]:
                    rows.append({"id": n["id"], "certificate_inspection": {"available": True, "observedAt": now(), "certificates": []}})
                    continue
                raw = command(["ssh", "-o", "BatchMode=yes", "--", n["ssh"], "sudo", "sh", "-s", "--", "control-plane"], script)
                rows.append({"id": n["id"], "certificate_inspection": json.loads(raw)})
        else:
            rows = self.sql("SELECT COALESCE(json_agg(json_build_object('id',id,'certificate_inspection',certificate_inspection)), '[]'::json) FROM nodes WHERE cluster_id='" + self.c["cluster_id"] + "'::uuid")
        require({r["id"] for r in rows} == {n["id"] for n in self.c["nodes"]}, "certificate_node_inventory_mismatch")
        result = {}
        for n in self.c["nodes"]:
            observation = next(r["certificate_inspection"] for r in rows if r["id"] == n["id"])
            require(observation and observation["available"], "certificate_inspection_unavailable")
            observed = timestamp(observation["observedAt"])
            require(after <= observed <= time.time() + 60 and time.time() - observed <= self.c.get("observation_max_age_seconds", 10800), "certificate_inspection_stale")
            items = {x["credential"]: x for x in observation["certificates"]}
            required = (CA | LEAF if n["control_plane"] else set()) | (set() if manual else {"kubelet-client", "kubelet-serving"})
            require(required <= items.keys(), "certificate_inventory_incomplete")
            for name, x in items.items():
                require(name in CATALOG, "unsupported_certificate")
                if name == "super-admin" and x["state"] == "missing":
                    continue
                require(x["state"] == "available" and HEX.fullmatch(x["certificateSHA256"]), "certificate_unavailable")
                expiry = timestamp(x["notAfter"])
                require(expiry > time.time(), "certificate_expired")
                result[(n["id"], name)] = {"sha256": x["certificateSHA256"], "not_after": x["notAfter"]}
        require_distinct_node_certificates(result, {n["id"] for n in self.c["nodes"] if n["control_plane"]})
        return result

    def temporal(self, action, workflow, *args):
        t = self.c["temporal"]
        return json.loads(command(["temporal", "workflow", action, "--address", t["address"], "--namespace", t["namespace"],
                                   "--workflow-id", workflow, "--output", "json", *args]))

    def workflow(self, workflow, expected_type):
        description = self.temporal("describe", workflow)
        info = description["workflowExecutionInfo"]
        require(info["execution"]["workflowId"] == workflow, "workflow_identity_mismatch")
        require(info["type"]["name"] == expected_type, "unsupported_workflow")
        status = info["status"].removeprefix("WORKFLOW_EXECUTION_STATUS_")
        require(status in {"RUNNING", "COMPLETED"}, "workflow_failed")
        return status

    def query(self, expression, evaluation_time=None):
        parameters = {"query": expression}
        if evaluation_time is not None:
            parameters["time"] = evaluation_time
        response = self.http(self.c["prometheus_url"], "/api/v1/query?" + urllib.parse.urlencode(parameters), "prometheus_authorization_file")
        require(response["status"] == "success" and response["data"]["resultType"] == "vector", "metric_query_failed")
        return response["data"]["result"]

    def metric_samples(self, selector, evaluation_time, after):
        # Instant-query value[0] is the evaluation time, including during lookback.
        # timestamp(selector)'s value is the actual source sample time.
        # Fetch both in one evaluation so ingestion between HTTP requests cannot
        # pair an old value with a newer sample timestamp at the same query instant.
        tag = "nvke_demo_sample"
        expression = ('label_replace(' + selector + ', "' + tag + '", "value", "", "") or '
                      'label_replace(timestamp(' + selector + '), "' + tag + '", "timestamp", "", "")')
        values, sampled = {}, {}
        for row in self.query(expression, evaluation_time):
            labels = dict(row["metric"])
            kind = labels.pop(tag, None)
            require(kind in {"value", "timestamp"}, "metric_sample_kind_invalid")
            labels.pop("__name__", None)
            identity = tuple(sorted(labels.items()))
            indexed = values if kind == "value" else sampled
            require(identity not in indexed, "metric_series_duplicate")
            indexed[identity] = metric_number(row["value"][1])
        require(values.keys() == sampled.keys(), "metric_sample_inventory_mismatch")
        require(all(after <= sample <= evaluation_time for sample in sampled.values()), "metric_sample_stale")
        return [(dict(labels), value, sampled[labels]) for labels, value in values.items()]

    def metrics(self, certificates, after=0):
        evaluation_time = time.time()
        cluster = json.dumps(self.c["cluster_name"])
        nodes = {n["name"] for n in self.c["nodes"]}
        def samples(suffix):
            return self.metric_samples('nvke_controller_' + suffix + '{tenant_cluster=' + cluster + '}', evaluation_time, after)
        fresh = samples("certificate_inspection_available")
        require({labels["node"] for labels, _value, _sample in fresh} == nodes and
                all(value == 1 for _labels, value, _sample in fresh), "inspection_metric_unavailable")
        expected = {(n["name"], credential): int(timestamp(value["not_after"])) for n in self.c["nodes"]
                    for (node_id, credential), value in certificates.items() if node_id == n["id"]}
        for suffix in ("certificate_not_after_timestamp_seconds", "certificate_observation_available"):
            found = {}
            for labels, value, _sample in samples(suffix):
                require(labels["node"] in nodes and labels["credential"] in CATALOG, "certificate_metric_inventory_mismatch")
                found.setdefault((labels["node"], labels["credential"]), []).append(value)
            for key, expiry in expected.items():
                require(key in found, "certificate_metric_missing")
                require(all(value == (expiry if suffix.endswith("timestamp_seconds") else 1) for value in found[key]),
                        "certificate_metric_snapshot_mismatch")
                require(expiry > time.time(), "certificate_metric_expired")
        inspection_ages = samples("certificate_observation_age_seconds")
        require({labels["node"] for labels, _value, _sample in inspection_ages} == nodes, "inspection_metric_unavailable")
        csr_ages = samples("serving_csr_observation_age_seconds")
        require(csr_ages, "csr_metric_missing")
        failures = {tuple(sorted((k, v) for k, v in row["metric"].items() if k != "__name__")): metric_number(row["value"][1])
                    for row in self.query('nvke_controller_credential_audit_writes_total{result=~"sink_failed|invalid_event|rate_limited"}',
                                          evaluation_time)}
        if not after:
            self.audit_failures_before, self.audit_failures_before_at = failures, time.time()
        # The controller creates every result series at zero; unchanged inspections
        # write nothing. The baseline records each failure series (by its labels,
        # including the pod or instance Prometheus adds) and the baseline time. Any
        # failed or dropped write since the baseline cannot be undone, so the lane
        # ends at once unless every later sample has exactly the baseline series
        # with the baseline values: a rollout or replica change alters the series
        # set and fails. An in-place restart that refills a series to its old value
        # keeps the set, so after the mutation also require no reset in any series
        # since the baseline; the window starts at the stored baseline time.
        # After the mutation, which writes audit events, require positive stored evidence.
        require(failures == self.audit_failures_before, "audit_writes_failed_during_lane")
        if after:
            window = max(1, math.ceil(time.time() - self.audit_failures_before_at))
            resets = self.query('sum(resets(nvke_controller_credential_audit_writes_total'
                                '{result=~"sink_failed|invalid_event|rate_limited"}[' + str(window) + 's]))')
            require(not resets or metric_number(resets[0]["value"][1]) == 0, "audit_writes_failed_during_lane")
        stored = self.query('sum(nvke_controller_credential_audit_writes_total{result="stored"})')
        require(len(stored) == 1 and (not after or metric_number(stored[0]["value"][1]) > 0),
                "audit_sink_unhealthy_or_unobserved")
        rules = self.http(self.c["prometheus_url"], "/api/v1/rules?type=alert", "prometheus_authorization_file")
        require(rules["status"] == "success", "alert_evaluator_unavailable")
        loaded = {r["name"] for g in rules["data"]["groups"] for r in g["rules"] if r.get("health") == "ok"}
        require({"NVKECertificateExpiring", "NVKECertificateRenewalFailed", "NVKEEtcdKeyRotationStuck"} <= loaded, "alert_rules_not_loaded")
        # Every replica must describe a successful observation after completion.
        # Age gauges are frozen between scrapes, so include elapsed scrape age.
        checked_at = time.time()
        for ages, limit, reason in ((inspection_ages, self.c.get("observation_max_age_seconds", 10800), "inspection_metric_stale"),
                                   (csr_ages, 300, "csr_metric_stale")):
            require(all(age >= 0 and sampled - age >= after and 0 <= age + checked_at - sampled <= limit
                        for _labels, age, sampled in ages), reason)

    def alert(self):
        expr = 'ALERTS{alertname="NVKECertificateExpiring",alertstate="firing",tenant_cluster=' + json.dumps(self.c["cluster_name"]) + ',node=' + json.dumps(self.c["alert_node"]) + ',credential=' + json.dumps(self.c["alert_credential"]) + '}'
        return bool(self.query(expr))

    def serving_certificates(self, certificates):
        # The proof is the freshly inspected installed certificate that the kubelet actually serves.
        # kube-controller-manager deletes approved and issued CSRs after about an hour, and these
        # workflows do not create serving CSRs, so a matching CSR is evidence only while still listed.
        csrs = self.kubejson("get", "csr")["items"]
        evidence = []
        for n in self.c["nodes"]:
            fingerprint = certificates[(n["id"], "kubelet-serving")]["sha256"]
            csr_observed = False
            for csr in csrs:
                spec, status = csr["spec"], csr.get("status", {})
                if spec.get("signerName") != "kubernetes.io/kubelet-serving" or spec.get("username") != "system:node:" + n["name"]:
                    continue
                if not status.get("certificate") or not any(x["type"] == "Approved" and x["status"] == "True" for x in status.get("conditions", [])):
                    continue
                der = command(["openssl", "x509", "-outform", "DER"], base64.b64decode(status["certificate"], validate=True))
                csr_observed = csr_observed or hashlib.sha256(der).hexdigest() == fingerprint
            context = ssl.create_default_context(cafile=self.c["kubelet_ca_file"])
            try:
                with socket.create_connection((n["kubelet_address"], 10250), timeout=10) as sock:
                    with context.wrap_socket(sock, server_hostname=n["kubelet_address"]) as tls:
                        served = hashlib.sha256(tls.getpeercert(binary_form=True)).hexdigest()
            except Exception:
                raise CheckError("kubelet_tls_verification_failed") from None
            require(served == fingerprint, "installed_certificate_not_served")
            evidence.append({"node_id": n["id"], "certificate_sha256": fingerprint, "installed_served": True,
                             "csr_observed": csr_observed})
        return evidence

    def _audit_events(self, workflow, since, manual=False):
        if manual:
            require(Path(self.c["manual_audit_file"]).exists(), "manual_audit_not_observed")
            events = [json.loads(line) for line in Path(self.c["manual_audit_file"]).read_text().splitlines() if line]
        else:
            events = self.sql("SELECT COALESCE(json_agg(event), '[]'::json) FROM credential_audit_events WHERE event #>> '{target,cluster_id}'='" + self.c["cluster_id"] + "' AND event #>> '{correlation,workflow_hash}'='" + audit_hash("workflow", workflow) + "'")
        accepted, seen = [], set()
        operations = manual_operation_hashes(self.c["manual_operation"]) if manual else set()
        for event in events:
            require(event["schema_version"] == "nke.credential-audit.v1" and HEX.fullmatch(event["event_id"]), "audit_schema_invalid")
            if timestamp(event["timestamp"]) < since:
                continue
            # event_id excludes the timestamp, so a duplicate is a repeated step
            # without its own step identity; export deduplication would merge it.
            require(event["event_id"] not in seen, "audit_event_duplicate")
            seen.add(event["event_id"])
            if manual:
                require(event.get("target", {}).get("cluster_hash") == audit_hash("cluster", self.c["kube_system_uid"]) and
                        event.get("correlation", {}).get("operation_hash") in operations,
                        "manual_audit_target_or_run_mismatch")
                require(event.get("actor", {}).get("kind") == "operator_reported", "manual_audit_actor_invalid")
            require(event["plane"] == ("t1" if manual else "t2"), "audit_plane_mismatch")
            require(event["credential_type"] == ("etcd_encryption_key" if self.c.get("scenario") == "etcd-key" else "kubeadm_leaf_bundle"), "audit_credential_mismatch")
            action, outcome = event["action"], event["outcome"]
            require(action in {"check", "approve", "renew", "rotate", "verify"} and outcome in {"success", "failure", "no_op"}, "audit_values_invalid")
            accepted.append(event)
        return accepted

    def renewal_outcomes(self, workflow, since):
        return self._renewal_outcomes(self._audit_events(workflow, since), workflow)

    def _renewal_outcomes(self, events, workflow):
        # V2 emits success for renewed and no_op/unchanged for an unchanged node.
        # The aggregate owning-state outcome only proves that some node renewed.
        expected = {n["id"] for n in self.c["nodes"] if n["control_plane"]}
        outcomes, attempts = {}, {}
        for event in events:
            if event["action"] != "renew":
                continue
            node = event.get("target", {}).get("node_id")
            require(node is None or node in expected, "renewal_audit_node_inventory_mismatch")
            correlation = event.get("correlation", {})
            operation = workflow if node is None else "k8s-cert-renewal-" + digest(workflow + "/" + node)
            require(correlation.get("operation_hash") == audit_hash("operation", operation) and
                    correlation.get("workflow_hash") == audit_hash("workflow", workflow) and
                    all(HEX.fullmatch(correlation.get(field, "")) for field in ("run_hash", "activity_hash")),
                    "renewal_audit_correlation_invalid")
            attempt = correlation.get("attempt")
            require(type(attempt) is int and attempt > 0, "renewal_audit_correlation_invalid")
            stage = (node, correlation["run_hash"], correlation["activity_hash"])
            attempts.setdefault(stage, {}).setdefault(attempt, set()).add((event["outcome"], event.get("reason_code")))
        aggregate = set()
        for (node, _run, _activity), stage_attempts in attempts.items():
            # A failed legacy process can be retried by this same activity.
            # Attempt numbers cannot order distinct activities or workflow runs.
            final = stage_attempts[max(stage_attempts)]
            require(len(final) == 1 and final <= {("success", "completed"), ("no_op", "unchanged")},
                    "renewal_audit_outcome_conflict")
            outcome = "renewed" if ("success", "completed") in final else "unchanged"
            if node is None:
                # CompleteCertificateRenewal also emits a cluster summary. It
                # cannot replace the executor's per-node renewal observations.
                aggregate.add(outcome)
                continue
            require(node not in outcomes or outcomes[node] == outcome, "renewal_audit_outcome_conflict")
            outcomes[node] = outcome
        require(outcomes.keys() == expected, "per_node_audit_coverage_incomplete")
        require("renewed" in outcomes.values(), "scheduled_cycle_did_not_renew")
        require(not aggregate or aggregate == {"renewed"}, "renewal_audit_outcome_conflict")
        return outcomes

    def audit(self, workflow, since, manual=False):
        events = self._audit_events(workflow, since, manual)
        outcomes = self._renewal_outcomes(events, workflow) if self.c.get("scenario") == "scheduled-renewal" else None
        # Rotation emits check/phase and completed rotate events. The storage
        # verifier supplies separate live verification; do not invent a ledger action.
        required = {"check", "rotate"} if self.c.get("scenario") == "etcd-key" else {"check", "renew", "verify"}
        require(required <= {x["action"] for x in events if x["outcome"] == "success"}, "audit_coverage_incomplete")
        if self.c.get("scenario") == "etcd-key":
            require({"prepare", "add_secondary", "promote_primary", "rewrite", "remove_old_keys", "finalize"} <=
                    {e.get("phase") for e in events if e["action"] == "check" and e["outcome"] == "success"}, "audit_phase_coverage_incomplete")
        else:
            for n in self.c["nodes"]:
                if not n["control_plane"]:
                    continue
                target_field = "node_hash" if manual else "node_id"
                target_value = audit_hash("node", n["name"]) if manual else n["id"]
                node_events = [e for e in events if e.get("target", {}).get(target_field) == target_value]
                renewal = "no_op" if outcomes and outcomes[n["id"]] == "unchanged" else "success"
                require(any(e["action"] == "renew" and e["outcome"] == renewal for e in node_events) and
                        any(e["action"] == "verify" and e["outcome"] == "success" for e in node_events),
                        "per_node_audit_coverage_incomplete")
        return [{"event_id": e["event_id"], "action": e["action"], "outcome": e["outcome"]} for e in events]


class Traffic:
    """Real read requests, including during mutation; only counters leave memory."""
    def __init__(self, live, proof=None):
        self.live, self.stop = live, threading.Event()
        self.proof = proof
        self.samples, self.errors, self.max_latency = 0, 0, 0.0
        self.secret_errors = 0
        self.target_mismatches = 0
        self.thread = threading.Thread(target=self.run, daemon=True)

    def run(self):
        while not self.stop.is_set():
            start = time.monotonic()
            try:
                # Kubectl may refresh credentials from the context, but every sample stays on the approved cluster.
                raw = self.live.kubectl("--server=" + self.live.c["api_server"], "get", "--raw=/api/v1/namespaces/kube-system")
                uid = json.loads(raw)["metadata"]["uid"]
            except Exception:
                self.errors += 1
            else:
                if uid != self.live.c["kube_system_uid"]:
                    self.target_mismatches += 1
            if self.proof:
                try:
                    self.proof.read_all()
                except Exception:
                    self.secret_errors += 1
            self.max_latency = max(self.max_latency, time.monotonic() - start)
            self.samples += 1
            self.stop.wait(self.live.c.get("traffic_interval_seconds", 2))

    def finish(self):
        self.stop.set()
        # The in-flight iteration is bounded by its per-command timeouts; wait for it rather than guess a second deadline.
        self.thread.join()
        require(self.samples >= 2, "traffic_observation_incomplete")
        return {"samples": self.samples, "errors": self.errors, "secret_read_errors": self.secret_errors,
                "target_mismatches": self.target_mismatches, "max_latency_seconds": round(self.max_latency, 3)}


NODE_UNIQUE = ("apiserver", "etcd-server", "etcd-peer")


def require_distinct_node_certificates(certificates, control_planes):
    """Each control plane serves its own apiserver and etcd leaves. Equal fingerprints
    on two nodes mean one node's observation was attributed to another, for example
    through a copy-pasted SSH destination."""
    for credential in NODE_UNIQUE:
        fingerprints = [certificates[(node, credential)]["sha256"] for node in control_planes if (node, credential) in certificates]
        require(len(fingerprints) == len(set(fingerprints)), "node_certificate_attribution_ambiguous")


def compare_certificates(before, after, changed, renewed_nodes=None):
    require(before.keys() == after.keys(), "certificate_inventory_changed")
    for key, old in before.items():
        new = after[key]
        if key[1] in CA:
            require(new == old, "ca_certificate_changed")
        elif key[1] in LEAF or key[1] == "super-admin":
            if changed and (renewed_nodes is None or key[0] in renewed_nodes):
                require(new["sha256"] != old["sha256"] and timestamp(new["not_after"]) > timestamp(old["not_after"]), "leaf_certificate_not_renewed")
            else:
                require(new == old, "unexpected_leaf_certificate_change")


def public_snapshot(snapshot):
    return [{"node_id": node, "credential": credential, **value} for (node, credential), value in sorted(snapshot.items())]


def wait_for(check, deadline):
    last = "observation_incomplete"
    while time.monotonic() < deadline:
        try:
            return check()
        except CheckError as error:
            last = str(error)
            if last in {"workflow_failed", "workflow_identity_mismatch", "unsupported_workflow", "cycle_not_scheduled",
                        "cycle_not_scheduler_named", "cycle_started_before_traffic", "manual_marker_invalid", "audit_schema_invalid", "audit_values_invalid",
                        "audit_event_duplicate", "scheduled_cycle_did_not_renew",
                        "notification_order_invalid", "notification_refired_before_completion",
                        "audit_writes_failed_during_lane"}:
                raise
        time.sleep(2)
    raise CheckError(last)


def matching_receipts(c, since):
    entries = [json.loads(line) for line in Path(c["alert_receipts_file"]).read_text().splitlines() if line]
    selected = [e for e in entries if e.get("schema") == "nke.alert-receipt.v1" and
                e.get("cluster_hash") == digest(c["cluster_name"]) and e.get("node_hash") == digest(c["alert_node"]) and
                e.get("credential") == c["alert_credential"] and timestamp(e["received_at"]) >= since]
    require(all(HEX.fullmatch(e.get("receipt_id", "")) for e in selected), "notification_receipt_invalid")
    return selected


def firing_receipt_received(c, since, until):
    return any(e["status"] == "firing" and timestamp(e["received_at"]) <= until for e in matching_receipts(c, since))


def alert_receipts(c, since, mutation_at, completed_at):
    selected = matching_receipts(c, since)
    failure, pending = None, False
    for receipt in sorted({e["receipt_id"] for e in selected}):
        same = [e for e in selected if e["receipt_id"] == receipt]
        firing = [e for e in same if e["status"] == "firing" and timestamp(e["received_at"]) <= mutation_at]
        # Renewal is one node at a time, so the alert node's inspection can resolve
        # the alert before the last node finishes; accept any resolution after
        # the mutation started, but the alert must not fire again before completion.
        resolved = [e for e in same if e["status"] == "resolved" and timestamp(e["received_at"]) >= mutation_at]
        if firing and resolved:
            pair = [max(firing, key=lambda e: timestamp(e["received_at"])), min(resolved, key=lambda e: timestamp(e["received_at"]))]
            resolved_at = timestamp(pair[1]["received_at"])
            try:
                require(timestamp(pair[0]["received_at"]) < resolved_at, "notification_order_invalid")
                require(not any(e["status"] == "firing" and resolved_at < timestamp(e["received_at"]) <= completed_at for e in same),
                        "notification_refired_before_completion")
            except CheckError as error:
                # Another receipt may still carry a complete delivery; keep the
                # first specific reason so a single candidate still reports it.
                failure = failure or error
                continue
            return [{"status": e["status"], "received_at": e["received_at"], "receipt_id": e["receipt_id"]} for e in pair]
        # Pending: fired by the mutation and not resolved since its latest such firing.
        pending = pending or (bool(firing) and not any(
            e["status"] == "resolved" and timestamp(e["received_at"]) > max(timestamp(f["received_at"]) for f in firing) for e in same))
    # A receipt that has fired may still resolve validly, so a specific (terminal)
    # failure is reported only once no such receipt is pending.
    if failure and not pending:
        raise failure
    raise CheckError("notification_delivery_incomplete")


def run(c, scenario, approve, live_factory=Live):
    validate_config(c, scenario)
    c["scenario"] = scenario
    start = time.time()
    evidence = {"schema": "nke.rotation-evidence.v1", "scenario": scenario, "status": "failed", "live": live_factory is Live,
                "source_revision": command(["git", "-C", str(SOURCE_ROOT), "rev-parse", "HEAD"]).decode().strip(),
                "source_dirty": bool(command(["git", "-C", str(SOURCE_ROOT), "status", "--porcelain"])),
                "cluster_id": c["cluster_id"], "kube_system_uid": c["kube_system_uid"], "profile": c["profile"],
                "api_endpoint_sha256": digest(c["api_server"]),
                "control_planes": sum(n["control_plane"] for n in c["nodes"]), "nodes": len(c["nodes"]),
                "started_at": now(), "checks": []}
    live, traffic = live_factory(c), None
    mutation_started = False
    def passed(name):
        evidence["checks"].append(name)
    deadline = time.monotonic() + c.get("timeout_seconds", 7200)
    try:
        require(evidence["source_revision"] == c["deployment_source_revision"], "deployment_revision_does_not_match_demo")
        evidence["deployment_source_revision"] = c["deployment_source_revision"]
        live.identity()
        baseline_health = live.health()
        passed("target_identity_and_health")
        manual = scenario == "manual-t1"
        evidence["deployed_versions_before"] = live.versions(manual)
        passed("declared_deployed_versions_match")
        baseline = live.certificates(manual=manual)
        evidence["certificates_before"] = public_snapshot(baseline)
        passed("public_certificate_inventory_before")
        if not manual:
            cluster = live.cluster()
            conditions = cluster["conditions"]
            require(not conditions.get("updateInProgress") and not conditions.get("deleting", {}).get("status") == "True", "mutation_conflict")
            # The terminal predicate requires updateFailed to be absent and renewal neither rejects nor clears it.
            require(not conditions.get("updateFailed"), "update_failed_present")
            require(conditions.get("provisioned", {}).get("status") == "True", "cluster_not_provisioned")
            wait_for(lambda: live.metrics(baseline), deadline)
            passed("fresh_metrics_and_loaded_alert_rules")
        if scenario == "alert-renewal":
            require(live.alert(), "expiry_alert_not_firing")
            target = next(n for n in c["nodes"] if n["name"] == c["alert_node"])
            expiry = timestamp(baseline[(target["id"], c["alert_credential"])]["not_after"])
            require(expiry > start, "expiry_alert_after_certificate_expiration")
            # The receiver must be running before this run; firing notification may precede start.
            receipts_since = c.get("alert_receipts_since")
            require(receipts_since is not None, "missing_notification_window")
            receipts_since = timestamp(receipts_since)
            require(receipts_since <= start and start - receipts_since <= 86400, "invalid_notification_window")
            # Only the bundled receiver writes receipts the collector accepts; without its
            # firing receipt now, the lane could only fail after the renewal.
            require(Path(c["alert_receipts_file"]).is_file() and firing_receipt_received(c, receipts_since, time.time()),
                    "firing_notification_not_received")
            passed("expiry_alert_firing_before_mutation")
        require(approve == c["cluster_id"], "operator_approval_required")
        workflow = "operator-rotation-demo-" + str(uuid.uuid4())
        proof = None
        if scenario == "etcd-key":
            require(cluster.get("etcdEncrypted") is True, "etcd_encryption_not_enabled")
            proof = EtcdProof(live)
            proof.create()
            evidence["storage_header_before_sha256"] = proof.verify()
            passed("preexisting_secret_readable_encrypted")
        traffic_started_at = time.time()
        traffic = Traffic(live, proof)
        traffic.thread.start()
        mutation_at = time.time()
        if scenario in {"etcd-key", "forced-renewal", "alert-renewal"}:
            evidence["workflow_hash"] = audit_hash("workflow", workflow)
            mutation_started = True
        if scenario == "etcd-key":
            live.temporal("start", workflow, "--type", "RotateEtcdEncryptionKey", "--task-queue", c["temporal"]["task_queue"],
                          "--id-reuse-policy", "RejectDuplicate", "--id-conflict-policy", "Fail", "--input", json.dumps({"clusterID": c["cluster_id"]}))
        elif scenario in {"forced-renewal", "alert-renewal"}:
            live.temporal("start", workflow, "--type", "RenewK8sCertsV2", "--task-queue", c["temporal"]["task_queue"],
                          "--id-reuse-policy", "RejectDuplicate", "--id-conflict-policy", "Fail", "--input",
                          json.dumps({"clusterID": c["cluster_id"], "thresholdDays": c.get("threshold_days", 30), "force": True}))
        elif scenario == "scheduled-renewal":
            prior = conditions.get("certificateRenewal", {}).get("cycleID")
            def scheduled():
                state = live.cluster()["conditions"].get("certificateRenewal", {})
                require(state.get("cycleID") != prior and state.get("cycleID"), "scheduled_cycle_not_observed")
                require(not state.get("forced") and (state.get("scheduledFor") or state["cycleID"].endswith("-initial")), "cycle_not_scheduled")
                require(state["cycleID"].startswith("renew-k8s-certs-" + c["cluster_id"] + "-"), "cycle_not_scheduler_named")
                # A cycle accepted during the baseline phase ran without observed traffic.
                require(timestamp(state["startedAt"]) >= traffic_started_at, "cycle_started_before_traffic")
                return state["cycleID"]
            workflow = wait_for(scheduled, deadline)
        else:
            # The operator follows the T1 guide separately and creates this marker only afterwards.
            require(not Path(c["manual_completion_file"]).exists(), "manual_marker_already_exists")
            print("T1 baseline recorded and API observation running. Begin the approved manual runbook now.", flush=True)
            def completed():
                require(Path(c["manual_completion_file"]).exists(), "manual_completion_not_observed")
                # A shell redirect creates the file before writing it; keep polling
                # until the content is complete.
                try:
                    marker = read_json(c["manual_completion_file"])
                except ValueError:
                    raise CheckError("manual_completion_not_observed") from None
                require(marker == {"cluster_id": c["cluster_id"], "kube_system_uid": c["kube_system_uid"], "completed": True}, "manual_marker_invalid")
            wait_for(completed, deadline)
        evidence["workflow_hash"] = audit_hash("workflow", workflow) if not manual else None
        if not manual:
            def terminal():
                require(live.workflow(workflow, "RotateEtcdEncryptionKey" if scenario == "etcd-key" else "RenewK8sCertsV2") == "COMPLETED", "workflow_not_terminal")
                state = live.cluster()["conditions"]
                field = "etcdKeyRotation" if scenario == "etcd-key" else "certificateRenewal"
                owner = "workflowID" if scenario == "etcd-key" else "cycleID"
                require(state.get(field, {}).get(owner) == workflow and state[field].get("state") == "succeeded", "owning_state_not_succeeded")
                require(not state.get("updateInProgress") and not state.get("updateFailed"), "mutation_lock_not_cleared")
                if scenario != "etcd-key":
                    require(state[field].get("outcome") == "renewed", "scheduled_cycle_did_not_renew")
            wait_for(terminal, deadline)
            passed("terminal_workflow_and_owning_state_success")
        completion = time.time()
        renewed_nodes = None
        if scenario == "scheduled-renewal":
            outcomes = wait_for(lambda: live.renewal_outcomes(workflow, start), deadline)
            renewed_nodes = {node for node, outcome in outcomes.items() if outcome == "renewed"}
            evidence["renewal_outcomes"] = [{"node_id": node, "outcome": outcome} for node, outcome in sorted(outcomes.items())]
            passed("scheduled_per_node_renewal_outcomes_observed")
        after = wait_for(lambda: live.certificates(after=completion, manual=manual), deadline)
        compare_certificates(baseline, after, scenario != "etcd-key", renewed_nodes)
        evidence["certificates_after"] = public_snapshot(after)
        passed("leaf_expiry_fingerprints_and_unchanged_cas")
        health = live.health()
        require(health["members"] == baseline_health["members"], "etcd_membership_changed")
        if scenario == "etcd-key":
            require(health["components"] == baseline_health["components"], "components_restarted_during_key_rotation")
            evidence["storage_header_after_sha256"] = proof.verify(evidence["storage_header_before_sha256"])
            passed("old_secret_rewritten_readable_and_api_primary_convergence")
        else:
            renewed_names = {n["name"] for n in c["nodes"] if renewed_nodes is None or n["id"] in renewed_nodes}
            require(health["components"].keys() == baseline_health["components"].keys(), "component_inventory_invalid")
            require(all(health["components"][key] != value for key, value in baseline_health["components"].items()
                        if key[0] in renewed_names), "renewed_components_not_replaced")
            require(all(health["components"][key] == value for key, value in baseline_health["components"].items()
                        if key[0] not in renewed_names), "unchanged_components_replaced")
            passed("renewed_static_pod_components_replaced")
        passed("nodes_ready_exact_etcd_membership_all_endpoints_healthy")
        evidence["deployed_versions_after"] = live.versions(manual)
        require(evidence["deployed_versions_after"] == evidence["deployed_versions_before"], "deployed_versions_changed_during_demo")
        if not manual:
            evidence["serving_certificates"] = live.serving_certificates(after)
            passed("serving_certificate_installed_and_served")
        else:
            evidence["manual_attribution"] = "operator_reported"
            evidence["excluded_checks"] = ["kubelet_inventory_and_csr_not_in_t1_runbook", "automatic_t2_metrics_and_workflows"]
        if not manual:
            wait_for(lambda: live.metrics(after, completion), deadline)
            passed("fresh_metrics_after_mutation")
        evidence["audit_events"] = wait_for(lambda: live.audit(workflow, start, manual), deadline)
        passed("required_audit_actions_observed")
        if scenario == "alert-renewal":
            wait_for(lambda: require(not live.alert(), "expiry_alert_not_resolved"), deadline)
            evidence["notification_receipts"] = wait_for(lambda: alert_receipts(c, receipts_since, mutation_at, completion), deadline)
            passed("expiry_alert_resolved_and_notifications_received")
        summary = traffic.finish()
        traffic = None
        evidence["traffic"] = summary
        require(not proof or summary.get("secret_read_errors", 0) == 0, "continuous_secret_read_or_payload_failed")
        require(summary["target_mismatches"] == 0, "traffic_target_mismatch")
        require(summary["errors"] / summary["samples"] <= c.get("max_api_error_fraction", 0) and
                summary["max_latency_seconds"] <= c.get("max_api_latency_seconds", 10), "api_traffic_budget_exceeded")
        if proof:
            live.kubectl("delete", "secret", proof.name, "-n", "default")
        passed("continuous_api_traffic_within_declared_budget")
        require(required_checks(scenario) <= set(evidence["checks"]), "evidence_checks_missing")
        evidence["status"] = "passed"
    except CheckError as error:
        evidence["reason"] = str(error)
    except Exception:
        evidence["reason"] = "collector_input_or_protocol_error"
    except KeyboardInterrupt:
        # Return the report so main records the failure and any started workflow.
        evidence["reason"] = "interrupted"
    finally:
        if traffic:
            try:
                evidence["traffic"] = traffic.finish()
            except CheckError:
                evidence["traffic"] = {"incomplete": True}
        evidence["mutation_started"] = mutation_started
        evidence["ended_at"] = now()
        # Never terminate/reset a workflow or clear ownership when observation fails.
    return evidence


class EtcdProof:
    def __init__(self, live):
        self.live = live
        self.name = "nvke-demo-public-" + uuid.uuid4().hex[:16]
        self.fixture = b"nvke-rotation-public-fixture"

    def create(self):
        self.live.kubectl("create", "secret", "generic", self.name, "-n", "default", "--from-literal=probe=" + self.fixture.decode())

    def header(self, name):
        pods = self.live.kubejson("get", "pods", "-n", "kube-system", "-l", "component=etcd")["items"]
        require(pods, "etcd_member_missing")
        raw = self.live.kubectl("exec", "-n", "kube-system", pods[0]["metadata"]["name"], "-c", "etcd", "--", "etcdctl",
                                "--endpoints=https://127.0.0.1:2379", "--cacert=/etc/kubernetes/pki/etcd/ca.crt",
                                "--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt", "--key=/etc/kubernetes/pki/etcd/healthcheck-client.key",
                                "get", "/registry/secrets/default/" + name, "--print-value-only")
        require(self.fixture not in raw, "plaintext_secret_storage")
        header = re.match(rb"k8s:enc:secretbox:v1:[A-Za-z0-9_.-]+:", raw[:128])
        require(header, "unsupported_or_missing_secret_storage")
        return hashlib.sha256(header.group()[:-1]).hexdigest()

    def verify(self, old=None):
        header = self.header(self.name)
        require(old is None or old != header, "preexisting_secret_not_rewritten")
        pods = self.live.kubejson("get", "pods", "-n", "kube-system", "-l", "component=kube-apiserver")["items"]
        expected = {n["name"] for n in self.live.c["nodes"] if n["control_plane"]}
        require({p["spec"]["nodeName"] for p in pods} == expected and len(pods) == len(expected), "api_server_inventory_incomplete")
        for index, pod in enumerate(pods):
            ip = pod["status"]["podIP"]
            server = "https://" + ("[" + ip + "]" if ":" in ip else ip) + ":6443"
            secret = self.live.kubejson("--server=" + server, "get", "secret", self.name, "-n", "default")
            require(base64.b64decode(secret["data"]["probe"], validate=True) == self.fixture, "preexisting_secret_payload_changed")
            if old:
                probe = self.name + "-" + str(index)
                self.live.kubectl("--server=" + server, "create", "secret", "generic", probe, "-n", "default", "--from-literal=probe=" + self.fixture.decode())
                try:
                    require(self.header(probe) == header, "api_server_primary_not_converged")
                finally:
                    self.live.kubectl("delete", "secret", probe, "-n", "default", "--ignore-not-found")
        # Caller removes the fixture after continuous observation has stopped.
        return header

    def read_all(self):
        pods = self.live.kubejson("get", "pods", "-n", "kube-system", "-l", "component=kube-apiserver")["items"]
        expected = {n["name"] for n in self.live.c["nodes"] if n["control_plane"]}
        require({p["spec"]["nodeName"] for p in pods} == expected and len(pods) == len(expected), "api_server_inventory_incomplete")
        for pod in pods:
            ip = pod["status"]["podIP"]
            server = "https://" + ("[" + ip + "]" if ":" in ip else ip) + ":6443"
            secret = self.live.kubejson("--server=" + server, "get", "secret", self.name, "-n", "default")
            require(base64.b64decode(secret["data"]["probe"], validate=True) == self.fixture, "continuous_secret_read_or_payload_failed")


def required_checks(scenario):
    """Check names a passed report for the scenario must contain; used by run completeness and aggregate."""
    required = {"target_identity_and_health", "declared_deployed_versions_match", "public_certificate_inventory_before",
                "leaf_expiry_fingerprints_and_unchanged_cas", "nodes_ready_exact_etcd_membership_all_endpoints_healthy",
                "required_audit_actions_observed", "continuous_api_traffic_within_declared_budget"}
    if scenario != "manual-t1":
        required |= {"terminal_workflow_and_owning_state_success", "fresh_metrics_and_loaded_alert_rules", "fresh_metrics_after_mutation",
                     "serving_certificate_installed_and_served"}
    if scenario == "scheduled-renewal":
        required.add("scheduled_per_node_renewal_outcomes_observed")
    if scenario == "alert-renewal":
        required |= {"expiry_alert_firing_before_mutation", "expiry_alert_resolved_and_notifications_received"}
    if scenario == "etcd-key":
        required |= {"preexisting_secret_readable_encrypted", "old_secret_rewritten_readable_and_api_primary_convergence"}
    else:
        required.add("renewed_static_pod_components_replaced")
    return required


def aggregate(paths):
    reports = [read_json(path) for path in paths]
    selected = {}
    for report in reports:
        require(report.get("schema") == "nke.rotation-evidence.v1" and report.get("scenario") in SCENARIOS, "invalid_evidence")
        require(isinstance(report.get("source_dirty"), bool), "invalid_evidence")
        require(report.get("status") in {"passed", "failed"}, "invalid_evidence_status")
        if report["status"] == "passed":
            require(required_checks(report["scenario"]) <= set(report.get("checks", [])), "evidence_checks_missing")
            versions = report.get("deployed_versions_before")
            require(isinstance(versions, list) and versions and all(
                isinstance(v, dict) and set(v) == {"component", "node_id", "reported_version_sha256"} and
                isinstance(v["reported_version_sha256"], str) and HEX.fullmatch(v["reported_version_sha256"])
                for v in versions), "evidence_deployed_versions_invalid")
        require(report["scenario"] not in selected, "duplicate_scenario")
        selected[report["scenario"]] = report
    missing = sorted(set(SCENARIOS) - selected.keys())
    passed = all(r.get("status") == "passed" and r.get("live") is True and not r["source_dirty"] for r in reports)
    # T1 is a different cluster; T2 scenarios must identify the same source, cluster and profile.
    t2 = [r for r in reports if r["scenario"] != "manual-t1"]
    require(len({(r["source_revision"], r["cluster_id"], r["kube_system_uid"], r["profile"]) for r in t2}) <= 1, "release_target_or_revision_mismatch")
    # A redeploy between lanes changes the observed build even when expected_versions is updated to match.
    builds = {json.dumps(sorted(json.dumps(v, sort_keys=True) for v in r["deployed_versions_before"]))
              for r in t2 if r["status"] == "passed"}
    require(len(builds) <= 1, "release_deployed_versions_mismatch")
    return {"schema": "nke.rotation-release.v1", "created_at": now(), "status": "passed" if passed and not missing else "incomplete",
            "missing_scenarios": missing, "scenarios": [{"scenario": r["scenario"], "status": r["status"],
                "evidence_sha256": hashlib.sha256(Path(p).read_bytes()).hexdigest()} for p, r in zip(paths, reports)],
            "qualification_limits": ["No CA rotation proof", "No failure/reset recovery proof", "Receiver observations depend on operator routing"]}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    demo = sub.add_parser("run")
    demo.add_argument("--config", required=True)
    demo.add_argument("--scenario", choices=SCENARIOS, required=True)
    demo.add_argument("--approve-cluster")
    demo.add_argument("--output", required=True)
    release = sub.add_parser("aggregate")
    release.add_argument("--output", required=True)
    release.add_argument("evidence", nargs="+")
    args = parser.parse_args(argv)
    os.umask(0o077)
    reserved = False
    try:
        # Reserve the exclusive private destination before any live collection or mutation.
        with os.fdopen(os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as output:
            reserved = True
            if args.action == "run":
                evidence = run(read_json(args.config), args.scenario, args.approve_cluster)
            else:
                evidence = aggregate(args.evidence)
            text = json.dumps(evidence, indent=2, sort_keys=True) + "\n"
            reserved = False  # Writing starts; from here a failure keeps whatever reached the file.
            output.write(text)
            output.flush()
            os.fsync(output.fileno())
        print("Rotation evidence: " + evidence["status"])
        return 0 if evidence["status"] == "passed" else 1
    except Exception as error:
        if reserved:
            # Nothing was serialized; release the path so a corrected rerun can use it.
            Path(args.output).unlink(missing_ok=True)
        reason = str(error) if isinstance(error, CheckError) else "collector_input_or_protocol_error"
        print("Rotation evidence could not be collected or written: " + reason, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
