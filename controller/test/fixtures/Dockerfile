# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

# Minimal CWE profiles image for CI (capd-e2e) and local Tilt runs.
#
# The Tiltfile builds this with build context test/fixtures/ and the chart's
# materialize-profiles initContainer copies /profiles into the shared volume
# that the app container mounts at PROFILES_DIR (profiles.path). Fleet profiles
# live in nke-site-deploy/cwe-profiles (NKX-13910); this tree is a sanitized
# fixture used only for tests.
FROM dockerhub.nvidia.com/alpine:3.21.7
RUN mkdir -p /profiles
COPY profiles/ /profiles/
