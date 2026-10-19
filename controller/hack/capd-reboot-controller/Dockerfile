# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# syntax=docker/dockerfile:1
# An ARG so CI can point the base at a Docker Hub mirror; see dockerhub_build_args.
ARG GOLANG_IMAGE=docker.io/golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d
FROM ${GOLANG_IMAGE}
ENV GOPROXY=https://proxy.golang.org|direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /usr/local/bin/capd-reboot-controller ./cmd/capd-reboot-controller
ENTRYPOINT ["/usr/local/bin/capd-reboot-controller"]
