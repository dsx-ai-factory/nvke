#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The etcd-encryption checks shared by run.sh and run-nico.sh. Source it; it
# defines functions only. The caller defines log, fail and defer.
#
# Two checks. The first reads a Secret straight out of etcd and looks at the
# bytes: secretbox envelope present, plaintext absent. It is the only way to
# tell "encryption configured" from "data encrypted". The second compares the
# key `bootstrap site` saved with the one each control-plane node runs on, by
# digest: the file is what an operator keeps, and it has to be the right key.
# Neither check prints key material or ciphertext.

# check_etcd_encrypted_at_rest KUBECONFIG KEY_FILE: KEY_FILE exists with mode
# 600, and a Secret written through KUBECONFIG's cluster is stored encrypted.
check_etcd_encrypted_at_rest() {
  local kubeconfig="$1" key_file="$2"
  log "checking that etcd data is encrypted at rest"
  [ -f "${key_file}" ] || fail "bootstrap site did not write ${key_file}"
  local key_mode
  key_mode=$(stat -c '%a' "${key_file}")
  [ "${key_mode}" = "600" ] || fail "${key_file} has mode ${key_mode}, want 600"

  local probe_name="nvkectl-e2e-encryption-probe"
  local plaintext_marker
  plaintext_marker="nvkectl-e2e-plaintext-marker-$(date +%s)"
  kubectl --kubeconfig "${kubeconfig}" -n default create secret generic "${probe_name}" \
    --from-literal=probe="${plaintext_marker}" >/dev/null
  local etcd_pod
  etcd_pod=$(kubectl --kubeconfig "${kubeconfig}" -n kube-system get pods -l component=etcd \
    -o jsonpath='{.items[0].metadata.name}')
  [ -n "${etcd_pod}" ] || fail "no etcd pod found in kube-system"
  # The raw value goes to a file, not a variable, so its bytes never reach the
  # log. Removed on exit like everything else this suite creates.
  local raw_value
  raw_value=$(mktemp)
  defer rm -f "${raw_value}"
  kubectl --kubeconfig "${kubeconfig}" -n kube-system exec "${etcd_pod}" -c etcd -- \
    etcdctl --endpoints=https://127.0.0.1:2379 \
      --cacert=/etc/kubernetes/pki/etcd/ca.crt \
      --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
      --key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
      get "/registry/secrets/default/${probe_name}" --print-value-only >"${raw_value}" 2>/dev/null ||
    fail "could not read the probe Secret out of etcd"
  # Positive control first: an empty read would pass the plaintext check for the
  # wrong reason.
  [ -s "${raw_value}" ] || fail "the probe Secret's etcd value is empty"
  ! grep -a -F -q "${plaintext_marker}" "${raw_value}" ||
    fail "the probe Secret is stored in etcd as plaintext"
  grep -a -q 'k8s:enc:secretbox:v1:' "${raw_value}" ||
    fail "the probe Secret's etcd value does not carry the secretbox envelope"
  rm -f "${raw_value}"
  kubectl --kubeconfig "${kubeconfig}" -n default delete secret "${probe_name}" >/dev/null
  log "PASS: Secrets are stored in etcd under the secretbox envelope"
}

# check_saved_key_matches_nodes KEY_FILE CONTAINER...: every CONTAINER, a
# control-plane node, runs the encryption key KEY_FILE holds.
check_saved_key_matches_nodes() {
  local key_file="$1"
  shift
  log "checking that the saved key is the key every control-plane node runs on"
  # The manifest is what `bootstrap site` wrote, so its shape is known: the key
  # is the base64 "active" field. Digests are compared, never values.
  local saved_active saved_digest node node_digest
  saved_active=$(sed -n 's/.*"active": "\([^"]*\)".*/\1/p' "${key_file}" | head -1)
  [ -n "${saved_active}" ] || fail "${key_file} carries no active key"
  saved_digest=$(printf '%s' "${saved_active}" | base64 -d | sha256sum | cut -d' ' -f1)
  for node in "$@"; do
    node_digest=$(nerdctl exec "${node}" \
      awk '/secret:/ {printf "%s", $2; exit}' /etc/kubernetes/kube-api-encryption/encryption-config.yaml \
      | sha256sum | cut -d' ' -f1)
    [ "${node_digest}" = "${saved_digest}" ] ||
      fail "${node} runs a different etcd encryption key than ${key_file} holds"
  done
  log "PASS: the saved etcd encryption key matches all $# control-plane nodes"
}
