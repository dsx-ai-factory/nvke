# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Minimal CWE profiles image for CI (capd-e2e) and local Tilt runs.
#
# The Tiltfile builds this with build context test/fixtures/ and the chart's
# materialize-profiles initContainer copies /profiles into the shared volume
# that the app container mounts at PROFILES_DIR (profiles.path). Fleet profiles
# live in nke-site-deploy/cwe-profiles; this tree is a sanitized fixture used
# only for tests.
#
# Defaults to Docker Hub so this builds for anyone with no NVIDIA network
# access, matching controller/Dockerfile and hack/nico-mock/Dockerfile.
# Internal builds override it. Hardcoding dockerhub.nvidia.com here made the
# image unbuildable outside the NVIDIA network -- including on the GitHub
# runner, which cannot resolve that host at all.
ARG BASE_REGISTRY=docker.io

FROM ${BASE_REGISTRY}/alpine:3.24.1
RUN mkdir -p /profiles
COPY profiles/ /profiles/
COPY runbooks/ /profiles/runbooks/
