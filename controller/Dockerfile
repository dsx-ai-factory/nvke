# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Base images for the image this repository ships. Fully qualified and
# digest-pinned in an ARG.
#
# Dependabot does NOT update these two. Its docker ecosystem reads an image
# named on a FROM and does not resolve an ARG default, so these digests are
# bumped by hand. (The SPIFFE base below is a direct FROM and is still covered.)
# See the header of hack/base-image-check.sh for the evidence and the trade-off
# in closing it.
#
# Internal builds override these rather than BASE_REGISTRY, for example
#   docker build --build-arg GOLANG_IMAGE=dockerhub.nvidia.com/golang:1.27.2@sha256:... .
# A digest is content-addressed, so a mirror serving the same content answers
# to the same digest.
ARG GOLANG_IMAGE=docker.io/golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8
ARG ALPINE_IMAGE=docker.io/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

FROM ghcr.io/spiffe/k8s-spiffe-workload-jwt-exec-auth:1.0.0@sha256:e30e90774e5951f84eb6dfb36f972fead08b64c3bfdc0912c5b15f988e79de3a AS spiffe-jwt-exec-auth

# Set up golang build container
FROM ${GOLANG_IMAGE} AS builder
ENV GOPROXY=https://proxy.golang.org|direct
ARG TARGETOS
ARG TARGETARCH

# The build context is the REPOSITORY ROOT, not controller/ (docker-build-local
# passes it): the controller is a member of the root Go workspace, and the
# build needs go.work and the sibling modules (../agent, ./api). Public Go
# modules are downloaded into the builder's module cache on first build.
WORKDIR /workspace
COPY . /workspace

# Build. Use make to ensure consistency between dev and CI environments.
ARG BUILD_VERSION="v0.0.0"
ARG GIT_VERSION=""
WORKDIR /workspace/controller
RUN echo "BUILD_VERSION=${BUILD_VERSION} GIT_VERSION=${GIT_VERSION} TARGETOS=${TARGETOS} TARGETARCH=${TARGETARCH}"
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} BUILD_VERSION=${BUILD_VERSION} GIT_VERSION=${GIT_VERSION} make build

########################################################################
# Multi-state-final build
# This should be alpine for us to test & debug
FROM ${ALPINE_IMAGE}
# Runtime + in-container debug tooling, installed in a single --no-cache layer
# so the apk index is not shipped. Documentation (*-doc) packages and packages
# with no function inside this pod (binutils, pciutils, usbutils) are
# intentionally excluded to minimize the image's CVE surface.
#
# apk add leaves packages the base tag already carries at their old version,
# and Alpine patches a branch without always cutting a new tag. So upgrade
# first, or dependencies nobody names here (libcrypto3, libssl3) go stale.
RUN apk upgrade --no-cache && \
    apk add --update --no-cache \
    ca-certificates \
    bash \
    bash-completion \
    curl \
    gcompat \
    sed \
    attr \
    dialog \
    grep \
    util-linux \
    findutils \
    readline

WORKDIR /
COPY --from=builder /workspace/controller/bin/controller /usr/local/bin/controller
COPY --from=spiffe-jwt-exec-auth /ko-app/cmd /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth

# OS/config profiles are operator-supplied at runtime (profilesDir), not baked
# into the image.
RUN mkdir -p /profiles

# Include both the project license and generated dependency notices.
COPY LICENSE NOTICE /licenses/
COPY ./controller/THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

## https://github.com/opencontainers/image-spec/blob/main/annotations.md
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="controller" \
      org.opencontainers.image.source="https://github.com/dsx-ai-factory/nvke" \
      org.opencontainers.image.description="NVKE Cloud Workflow Engine"

EXPOSE 8080

CMD ["controller"]
