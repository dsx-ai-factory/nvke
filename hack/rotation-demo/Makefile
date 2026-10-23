# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
.PHONY: test lint
test:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -p 'test_*.py' -v
lint:
	python3 -c 'from pathlib import Path; [compile(p.read_text(), str(p), "exec") for p in Path(".").glob("*.py")]'
	bash -n demo.sh
	sh -n public-certificates.sh
	shellcheck demo.sh public-certificates.sh
