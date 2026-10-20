#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Cross-verifies this module's DSSE verdict output with the cosign CLI: the
# tagged test signs a verdict in-process, wraps it in a sigstore bundle v0.3
# and has cosign verify it against the blob (and reject it against different
# bytes). Proves the emitted envelopes are what the sigstore ecosystem
# accepts, not merely what pkg/verify accepts.
#
# Needs cosign v3+ on PATH (the test skips with a message otherwise).
# Verified against cosign v3.1.3.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
exec go test -tags crossverify -count=1 -run TestCosignCrossVerify -v ./pkg/verify/
