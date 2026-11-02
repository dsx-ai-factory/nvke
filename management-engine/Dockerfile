# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Share dependencies and source across the manager and REST builds.
FROM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS builder
ENV GOPROXY=https://proxy.golang.org|direct
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace/management-engine
# Copy the Go Modules manifests
COPY management-engine/go.mod management-engine/go.sum ./
COPY controller/api/go.mod controller/api/go.sum /workspace/controller/api/
# Cache dependencies separately so source edits do not download them again.
RUN go mod download

# Copy the Go source from the repository root (filtered by Dockerfile.dockerignore).
COPY management-engine/ ./
COPY controller/api/ /workspace/controller/api/

# Build for the requested platform; an unset TARGETARCH uses the builder architecture.
FROM builder AS manager-builder
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -trimpath -ldflags "-X main.version=${VERSION} -s -w -buildid=" -o manager ./cmd

FROM builder AS tenant-api-builder
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -trimpath -ldflags "-X main.version=${VERSION} -s -w -buildid=" -o tenant-api ./cmd/tenant-api

FROM builder AS operator-api-builder
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -trimpath -ldflags "-X main.version=${VERSION} -s -w -buildid=" -o operator-api ./cmd/operator-api

# Each runtime image contains only its service binary.
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS runtime
WORKDIR /
USER 65532:65532

FROM runtime AS tenant-api
COPY --from=tenant-api-builder /workspace/management-engine/tenant-api .
ENTRYPOINT ["/tenant-api"]

FROM runtime AS operator-api
COPY --from=operator-api-builder /workspace/management-engine/operator-api .
ENTRYPOINT ["/operator-api"]

# Keep manager last so builds without --target preserve the existing manager image.
FROM runtime AS manager
COPY --from=manager-builder /workspace/management-engine/manager .
ENTRYPOINT ["/manager"]
