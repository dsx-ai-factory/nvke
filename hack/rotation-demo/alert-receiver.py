# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Operator-run Alertmanager webhook receipt collector; stores public allowlisted fields."""
import argparse
import datetime
import hashlib
import http.server
import json
import os
import re
import sys
import threading

LOCK = threading.Lock()
# A peer that declares a body and then stalls must not hold a handler thread forever.
READ_TIMEOUT_SECONDS = 30


def receipts(body):
    results = []
    for alert in body.get("alerts", []):
        labels = alert.get("labels", {})
        if labels.get("alertname") != "NVKECertificateExpiring":
            continue
        credential = labels.get("credential")
        if credential not in {"apiserver", "apiserver-kubelet-client", "front-proxy-client", "apiserver-etcd-client",
                              "etcd-server", "etcd-peer", "etcd-healthcheck-client", "admin", "controller-manager", "scheduler"}:
            continue
        if alert.get("status") not in {"firing", "resolved"} or not labels.get("tenant_cluster") or not labels.get("node"):
            continue
        fingerprint = alert.get("fingerprint", "")
        if not re.fullmatch(r"[a-fA-F0-9]{16}", fingerprint):
            continue
        results.append({"schema": "nke.alert-receipt.v1", "received_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                        "receipt_id": hashlib.sha256(fingerprint.encode()).hexdigest(), "status": alert["status"],
                        "cluster_hash": hashlib.sha256(labels["tenant_cluster"].encode()).hexdigest(),
                        "node_hash": hashlib.sha256(labels["node"].encode()).hexdigest(), "credential": credential})
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bind", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9087)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    os.umask(0o077)
    # Append only to a regular private file; do not follow symlinks.
    flags = os.O_APPEND | os.O_WRONLY | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(args.output, flags, 0o600)
    if not os.path.isfile(args.output) or os.fstat(fd).st_mode & 0o077:
        os.close(fd)
        return 1
    class Handler(http.server.BaseHTTPRequestHandler):
        timeout = READ_TIMEOUT_SECONDS

        def do_POST(self):
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if self.path != "/alerts" or not 0 < size <= 1024 * 1024:
                    self.send_error(400)
                    return
                body = json.loads(self.rfile.read(size))
                rows = receipts(body)
                with LOCK:
                    for row in rows:
                        os.write(fd, (json.dumps(row, sort_keys=True) + "\n").encode())
                    os.fsync(fd)
                self.send_response(200)
                self.end_headers()
            except Exception:
                self.send_error(400)
        def log_message(self, *_args):
            pass
    try:
        http.server.ThreadingHTTPServer((args.bind, args.port), Handler).serve_forever()
    finally:
        os.close(fd)
    return 0


if __name__ == "__main__":
    sys.exit(main())
