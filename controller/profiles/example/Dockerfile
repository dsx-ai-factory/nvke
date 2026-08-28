# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG BASE_REGISTRY=docker.io

FROM ${BASE_REGISTRY}/alpine:3.24.1

RUN mkdir -p /profiles
COPY profiles/ /profiles/
COPY runbooks/ /profiles/runbooks/
