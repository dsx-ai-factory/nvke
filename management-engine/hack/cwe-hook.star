# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

if os.getenv("DEV_CWE", "") == "1":
    load_dynamic("../../management-engine/hack/cwe.Tiltfile")
