# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# syntax=docker/dockerfile:1
# An ARG so CI can point the base at a Docker Hub mirror; see dockerhub_build_args.
ARG GOLANG_IMAGE=docker.io/golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18
FROM ${GOLANG_IMAGE}
ENV GOPROXY=https://proxy.golang.org|direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /usr/local/bin/capd-reboot-controller ./cmd/capd-reboot-controller
ENTRYPOINT ["/usr/local/bin/capd-reboot-controller"]
