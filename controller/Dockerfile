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
#   docker build --build-arg GOLANG_IMAGE=dockerhub.nvidia.com/golang:1.27.3@sha256:... .
# A digest is content-addressed, so a mirror serving the same content answers
# to the same digest.
ARG GOLANG_IMAGE=docker.io/golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:debug-nonroot@sha256:d5563cc7f2f44313f332e91138cc8c6a158899afeeeab2fce3b0f9ccdb3cf9ee

FROM ghcr.io/spiffe/k8s-spiffe-workload-jwt-exec-auth:1.0.1@sha256:ac41337c043e986596dcabc961d569daf9f16a261fe307e665e0562238c30fb3 AS spiffe-jwt-exec-auth

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
# The runtime image has no shell to run mkdir in, so the empty mount point for
# operator-supplied profiles is made here and copied across.
RUN mkdir -p /out/profiles

########################################################################
# Runtime image: distroless static, which carries CA certificates, time zone
# data and the nonroot user, and no package manager or package database. The
# debug variant adds busybox, so `kubectl exec ... -- sh` still gives a shell
# with wget, grep, sed and find on PATH. It has no bash and no curl.
FROM ${RUNTIME_IMAGE}

WORKDIR /
COPY --from=builder /workspace/controller/bin/controller /usr/local/bin/controller
COPY --from=spiffe-jwt-exec-auth /ko-app/cmd /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth

# OS/config profiles are operator-supplied at runtime (profilesDir), not baked
# into the image.
COPY --from=builder /out/profiles /profiles

# Include both the project license and generated dependency notices.
COPY LICENSE NOTICE /licenses/
COPY ./controller/THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

## https://github.com/opencontainers/image-spec/blob/main/annotations.md
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="controller" \
      org.opencontainers.image.source="https://github.com/dsx-ai-factory/nvke" \
      org.opencontainers.image.description="NVKE Cloud Workflow Engine"

# The nonroot uid distroless images use. The chart sets the same uid and
# runAsNonRoot, which the kubelet can only verify against a numeric USER.
USER 65532:65532

EXPOSE 8080

# The debug base's entrypoint is /busybox/sh, which would run CMD as a shell
# script. Clear it so the container starts the controller as before, and a
# chart `command:` or `args:` override keeps its meaning.
ENTRYPOINT []
CMD ["controller"]
