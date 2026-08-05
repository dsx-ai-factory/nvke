# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

# syntax=docker/dockerfile:1
FROM dockerhub.nvidia.com/golang:1.25.12 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -o /workspace/bin/nico-mock ./cmd/nico-mock

FROM dockerhub.nvidia.com/alpine:3.21.6
RUN apk add --update --no-cache ca-certificates docker-cli

COPY --from=builder /workspace/bin/nico-mock /usr/local/bin/nico-mock

EXPOSE 8080

CMD ["nico-mock"]
