# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG BASE_REGISTRY=docker.io

FROM --platform=${BUILDPLATFORM} ${BASE_REGISTRY}/golang:1.26.6 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev

# The build context is the NKE repository root. The workspace vendor directory
# contains every dependency, so this build does not need source credentials.
WORKDIR /workspace
COPY . .
RUN cd cluster-api-provider-provisioned && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false \
      -ldflags "-X main.version=${BUILD_VERSION}" \
      -o /out/cluster-api-provider-provisioned \
      ./cmd/manager

FROM --platform=${BUILDPLATFORM} ${BASE_REGISTRY}/alpine:3.24.1 AS certificates
RUN apk add --no-cache ca-certificates

FROM ${BASE_REGISTRY}/alpine:3.24.1
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/cluster-api-provider-provisioned /usr/local/bin/cluster-api-provider-provisioned
COPY LICENSE NOTICE /licenses/
COPY controller/THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cluster-api-provider-provisioned"]
