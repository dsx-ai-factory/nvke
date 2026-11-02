#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Read-only T1 collector sent to a kubeadm node over the operator's SSH connection.
set -eu
umask 077
rotation_role="${1:?node role required}"
case "$rotation_role" in control-plane) ;; *) exit 1 ;; esac
rotation_tmp="$(mktemp -d)"
trap 'rm -rf "$rotation_tmp"' EXIT HUP INT TERM
rotation_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '{"available":true,"observedAt":"%s","certificates":[' "$rotation_time"
rotation_separator=''
certificate() {
    rotation_name="$1"
    rotation_path="$2"
    printf '%s' "$rotation_separator"
    rotation_separator=','
    if [ ! -f "$rotation_path" ]; then
        [ "$rotation_name" = super-admin ] || exit 1
        printf '{"credential":"super-admin","state":"missing"}'
        return
    fi
    # These commands output only public DER and public expiry. Private PEM blocks
    # in kubelet combined files are ignored by x509; never cat/copy those files.
    openssl x509 -in "$rotation_path" -outform DER > "$rotation_tmp/public.der" 2>/dev/null
    rotation_hash="$(sha256sum "$rotation_tmp/public.der" | cut -d ' ' -f 1)"
    rotation_end="$(openssl x509 -in "$rotation_path" -noout -enddate 2>/dev/null)"
    rotation_expiry="$(date -u -d "${rotation_end#notAfter=}" +%Y-%m-%dT%H:%M:%SZ)"
    printf '{"credential":"%s","state":"available","certificateSHA256":"%s","notAfter":"%s"}' \
        "$rotation_name" "$rotation_hash" "$rotation_expiry"
}
# T1 manual renewal excludes kubelet credentials and TLS bootstrap.
if [ "$rotation_role" = control-plane ]; then
    certificate apiserver /etc/kubernetes/pki/apiserver.crt
    certificate apiserver-kubelet-client /etc/kubernetes/pki/apiserver-kubelet-client.crt
    certificate front-proxy-client /etc/kubernetes/pki/front-proxy-client.crt
    certificate apiserver-etcd-client /etc/kubernetes/pki/apiserver-etcd-client.crt
    certificate etcd-server /etc/kubernetes/pki/etcd/server.crt
    certificate etcd-peer /etc/kubernetes/pki/etcd/peer.crt
    certificate etcd-healthcheck-client /etc/kubernetes/pki/etcd/healthcheck-client.crt
    certificate kubernetes-ca /etc/kubernetes/pki/ca.crt
    certificate front-proxy-ca /etc/kubernetes/pki/front-proxy-ca.crt
    certificate etcd-ca /etc/kubernetes/pki/etcd/ca.crt
    for rotation_config in admin super-admin controller-manager scheduler; do
        rotation_config_path="/etc/kubernetes/$rotation_config.conf"
        if [ ! -f "$rotation_config_path" ]; then
            certificate "$rotation_config" "$rotation_config_path"
            continue
        fi
        # jsonpath selects only public certificate data; command output and key
        # fields from the kubeconfig never enter the collector output.
        kubectl config view --kubeconfig "$rotation_config_path" --raw --minify \
            -o 'jsonpath={.users[0].user.client-certificate-data}' 2>/dev/null \
            | base64 -d > "$rotation_tmp/client.crt"
        certificate "$rotation_config" "$rotation_tmp/client.crt"
    done
fi
printf ']}\n'
