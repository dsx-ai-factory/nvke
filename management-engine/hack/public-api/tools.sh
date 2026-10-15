#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Resolve pinned generators and annotation schemas so public artifacts reproduce across environments.
set -euo pipefail

module_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tool_dir="${module_root}/bin"
go_command=${GO:-go}
mkdir -p "$tool_dir"

# ensure_tool reuses or installs the pinned executable so generation uses the reviewed toolchain.
ensure_tool() {
    local name=$1 module=$2 version=$3 expected=$4
    if [[ -x "$tool_dir/$name" ]] && [[ $("$tool_dir/$name" --version 2>&1) == "$expected" ]]; then
        return
    fi
    local existing
    existing=$(command -v "$name" || true)
    if [[ -n "$existing" ]] && [[ $("$existing" --version 2>&1) == "$expected" ]]; then
        cp "$existing" "$tool_dir/$name"
        return
    fi
    GOBIN="$tool_dir" "$go_command" install "$module@$version"
    if [[ $("$tool_dir/$name" --version 2>&1) != "$expected" ]]; then
        printf 'Unexpected version for %s\n' "$name" >&2
        exit 1
    fi
}

# module_version reads the runtime dependency pin so generators and imported schemas stay aligned.
module_version() {
    awk -v module="$1" '$1 == module { print $2; exit }' "$module_root/go.mod"
}

buf_version=$(awk '$1 == "BUF_VERSION" { print $3; exit }' "$module_root/../make/tool-versions.mk")
protobuf_version=$(module_version google.golang.org/protobuf)
gateway_module=github.com/grpc-ecosystem/grpc-gateway/v2
gateway_version=$(module_version "$gateway_module")
if [[ ${1:-} == --gateway-protos ]]; then
    "$go_command" mod download "$gateway_module@$gateway_version"
    "$go_command" list -m -f '{{.Dir}}/protoc-gen-openapiv2/options' "$gateway_module@$gateway_version"
    exit
fi
ensure_tool buf github.com/bufbuild/buf/cmd/buf "$buf_version" "${buf_version#v}"
ensure_tool protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go "$protobuf_version" "protoc-gen-go $protobuf_version"
ensure_tool protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2 'protoc-gen-go-grpc 1.6.2'
ensure_tool protoc-gen-grpc-gateway github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway "$gateway_version" \
    "Version $gateway_version, commit unknown, built at unknown"
ensure_tool protoc-gen-openapiv2 github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2 "$gateway_version" \
    "Version $gateway_version, commit unknown, built at unknown"
