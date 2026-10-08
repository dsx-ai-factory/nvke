# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# syntax=docker/dockerfile:1
# An ARG so CI can point the base at a Docker Hub mirror; see dockerhub_build_args.
ARG GOLANG_IMAGE=docker.io/golang:1.27.2-alpine@sha256:85dc1069ac644ea3c527b177303a406eb3358192816cd7f9e5848eb658851673
FROM ${GOLANG_IMAGE}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /usr/local/bin/capd-reboot-controller ./cmd/capd-reboot-controller
ENTRYPOINT ["/usr/local/bin/capd-reboot-controller"]
