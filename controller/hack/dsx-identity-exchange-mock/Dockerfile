# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG BASE_REGISTRY=docker.io

FROM ${BASE_REGISTRY}/golang:1.27.2 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY . /workspace
WORKDIR /workspace/controller
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -o /out/dsx-identity-exchange-mock ./hack/dsx-identity-exchange-mock

FROM ${BASE_REGISTRY}/alpine:3.24.1
COPY --from=builder /out/dsx-identity-exchange-mock /usr/local/bin/dsx-identity-exchange-mock
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/dsx-identity-exchange-mock"]
