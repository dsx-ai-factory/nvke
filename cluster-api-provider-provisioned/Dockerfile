# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Base images for the image this repository ships. Fully qualified and
# digest-pinned in an ARG, which is the form Dependabot can parse -- a FROM
# whose registry is a variable is not, which is why these followed a floating
# tag until now.
#
# Internal builds override these rather than BASE_REGISTRY, for example
#   docker build --build-arg GOLANG_IMAGE=dockerhub.nvidia.com/golang:1.27.1@sha256:... .
# A digest is content-addressed, so a mirror serving the same content answers
# to the same digest.
ARG GOLANG_IMAGE=docker.io/golang:1.27.1@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244
ARG ALPINE_IMAGE=docker.io/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev

# The build context is the NKE repository root. All Go dependencies are public;
# the builder downloads them as needed without source credentials.
WORKDIR /workspace
COPY . .
RUN cd cluster-api-provider-provisioned && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false \
      -ldflags "-X main.version=${BUILD_VERSION}" \
      -o /out/cluster-api-provider-provisioned \
      ./cmd/manager

FROM --platform=${BUILDPLATFORM} ${ALPINE_IMAGE} AS certificates
RUN apk add --no-cache ca-certificates

FROM ${ALPINE_IMAGE}
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/cluster-api-provider-provisioned /usr/local/bin/cluster-api-provider-provisioned
COPY LICENSE NOTICE /licenses/
COPY controller/THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cluster-api-provider-provisioned"]
