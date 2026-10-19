# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build the manager binary
FROM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS builder
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
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -trimpath -ldflags "-X main.version=${VERSION} -s -w -buildid=" -o manager ./cmd

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /workspace/management-engine/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
