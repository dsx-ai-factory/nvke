# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

FROM ghcr.io/spiffe/k8s-spiffe-workload-jwt-exec-auth:0.1.0@sha256:550fe75d87ed7a8a2312e27545a18ba21f93c75c68d13188baef1e7f6e77c8e8 AS spiffe-jwt-exec-auth

# Set up golang build container
FROM dockerhub.nvidia.com/golang:1.25.12 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Add special sauce for our private gitlab repos.
ENV GIT_TERMINAL_PROMPT=1
RUN go env -w GOPRIVATE='*.nvidia.com'

# Get dependencies.
COPY go.mod go.mod
COPY go.sum go.sum

# Copy in api go modules
COPY api/go.mod api/go.mod
COPY api/go.sum api/go.sum

# Cache dependencies before building and copying source to minimize downloads
# and prevent source changes from invalidating cached dependencies layer.

# path /kaniko/gitcreds is used as this works for kaniko builds in CI and local builds using docker
# For local builds, use make commands to build a docker image
# build secrets with id=gitcreds are expected to be passed in for localbuilds
RUN --mount=type=secret,id=gitcreds,dst=/kaniko/gitcreds \
    cp /kaniko/gitcreds /root/.netrc \
    && go mod download \
    && rm /root/.netrc

# Copy all source to build container, including /vendor, for faster builds (no downloads!)
COPY . /workspace

# Build. Use make to ensure consistency between dev and CI environments.
ARG BUILD_VERSION="v0.0.0"
ARG GIT_VERSION=""
RUN echo "BUILD_VERSION=${BUILD_VERSION} GIT_VERSION=${GIT_VERSION} TARGETOS=${TARGETOS} TARGETARCH=${TARGETARCH}"
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} BUILD_VERSION=${BUILD_VERSION} GIT_VERSION=${GIT_VERSION} make build

########################################################################
# Multi-state-final build
# This should be alpine for us to test & debug
FROM dockerhub.nvidia.com/alpine:3.21.7
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
COPY --from=builder /workspace/bin/cloud-workflow-engine /usr/local/bin/cloud-workflow-engine
COPY --from=spiffe-jwt-exec-auth /ko-app/cmd /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth

COPY ./profiles /profiles
COPY ./ui /ui

EXPOSE 8080

CMD ["cloud-workflow-engine"]
