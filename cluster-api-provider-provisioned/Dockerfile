# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Base images for the image this repository ships. Fully qualified and
# digest-pinned in an ARG.
#
# Dependabot does NOT update these. Its docker ecosystem reads an image named on
# a FROM and does not resolve an ARG default, so these digests are bumped by
# hand. Nothing in this file is covered: all three FROM lines below go through
# an ARG or a previous stage. See the header of hack/base-image-check.sh for the
# evidence and the trade-off in closing it.
#
# Internal builds override these rather than BASE_REGISTRY, for example
#   docker build --build-arg GOLANG_IMAGE=dockerhub.nvidia.com/golang:1.27.3@sha256:... .
# A digest is content-addressed, so a mirror serving the same content answers
# to the same digest.
ARG GOLANG_IMAGE=docker.io/golang:1.27.3@sha256:957861185dbf29d93668ace41e011d091c6e779953d47f0316ef2cc58e28fe2b
ARG ALPINE_IMAGE=docker.io/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS builder
ENV GOPROXY=https://proxy.golang.org|direct
ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev

# The build context is the NVKE repository root. All Go dependencies are public;
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
# The base tag carries the packages Alpine shipped when the tag was cut, and
# Alpine patches a branch without always cutting a new tag. Upgrade so the
# packages are current at build time. --no-cache keeps the apk index out of
# the image.
RUN apk upgrade --no-cache
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/cluster-api-provider-provisioned /usr/local/bin/cluster-api-provider-provisioned
COPY LICENSE NOTICE /licenses/
COPY controller/THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cluster-api-provider-provisioned"]
