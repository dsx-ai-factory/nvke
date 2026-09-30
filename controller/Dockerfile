# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

FROM ghcr.io/spiffe/k8s-spiffe-workload-jwt-exec-auth:0.3.0@sha256:6402624276b66ef7a733e126488397c9cf33c6706ee1f52a9c7ffad2a540aa7c AS spiffe-jwt-exec-auth

# Set up golang build container
FROM ${GOLANG_IMAGE} AS builder
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

# OS/config profiles are operator-supplied at runtime (PROFILES_DIR), not baked
# into the image.
RUN mkdir -p /profiles

# Include both the project license and generated dependency notices.
COPY LICENSE NOTICE /licenses/
COPY ./controller/THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

## https://github.com/opencontainers/image-spec/blob/main/annotations.md
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="controller" \
      org.opencontainers.image.source="https://github.com/dsx-ai-factory/nke" \
      org.opencontainers.image.description="NKE Cloud Workflow Engine"

EXPOSE 8080

CMD ["controller"]
