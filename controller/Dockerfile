# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

# Base image registry. Defaults to Docker Hub so the image builds for anyone
# with no NVIDIA network access. Internal builds override it, e.g.
#   docker build --build-arg BASE_REGISTRY=dockerhub.nvidia.com .
#
# The ${BASE_REGISTRY} images below are not digest-pinned, unlike the other
# Dockerfiles here. Dependabot cannot parse a FROM whose registry is a variable,
# so nothing would ever move the digest. Following the tag at least picks up
# in-place rebuilds, which is how base-OS CVE fixes ship under one patch tag.
#
# To pin them, put the whole reference in an ARG Dependabot can read
# (ARG GOLANG_IMAGE=docker.io/golang:1.26.6@sha256:...). That changes what
# callers override, so it needs agreement from whoever runs internal builds.
ARG BASE_REGISTRY=docker.io

FROM ghcr.io/spiffe/k8s-spiffe-workload-jwt-exec-auth:0.2.0@sha256:cffed83b451236bad4347110f116e0808a132b57b799acd7c9d8a4ce7cf6b781 AS spiffe-jwt-exec-auth

# Set up golang build container
FROM ${BASE_REGISTRY}/golang:1.26.6 AS builder
ARG TARGETOS
ARG TARGETARCH

# The build context is the REPOSITORY ROOT, not controller/ (docker-build-local
# passes it): the controller is a member of the root Go workspace, and the
# build needs go.work, the committed vendor/ tree, and the sibling modules
# (../agent, ./api). No credentials or module downloads are needed — every
# dependency comes from vendor/.
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
FROM ${BASE_REGISTRY}/alpine:3.21.7
# Runtime + in-container debug tooling, installed in a single --no-cache layer
# so the apk index is not shipped. Documentation (*-doc) packages and packages
# with no function inside this pod (binutils, pciutils, usbutils) are
# intentionally excluded to minimize the image's CVE surface (NKX-13061).
RUN apk add --update --no-cache \
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
COPY --from=builder /workspace/controller/bin/cloud-workflow-engine /usr/local/bin/cloud-workflow-engine
COPY --from=spiffe-jwt-exec-auth /ko-app/cmd /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth

# OS/config profiles are operator-supplied at runtime (PROFILES_DIR), not baked
# into the image.
RUN mkdir -p /profiles
COPY ./controller/ui /ui

# Include both the project license and generated dependency notices.
COPY LICENSE NOTICE /licenses/
COPY ./controller/THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

## https://github.com/opencontainers/image-spec/blob/main/annotations.md
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="cloud-workflow-engine" \
      org.opencontainers.image.source="https://github.com/NVIDIA/nke" \
      org.opencontainers.image.description="NKE Cloud Workflow Engine"

EXPOSE 8080

CMD ["cloud-workflow-engine"]
