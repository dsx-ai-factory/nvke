# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

##@ Help

.PHONY: help
help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

# DOCA 3.5.0 is the only release with native Ubuntu 24.04 and 26.04 builds.
# Bump here, update PACKAGES in fetch.sh if the dependency set changed, and run
# `make lock`; see README.md.
DOCA_VERSION ?= 3.5.0

ARCH ?= amd64
# The image's Ubuntu release. DOCA builds each release separately. Required by
# `debs`.
UBUNTU_SERIES ?=

# Where the .deb files are written. Required by `debs`.
OUT_DIR ?=

# Downloaded debs, reused across runs when they still verify.
DIST_DIR ?= $(CURDIR)/dist

# The committed lock. The fetch refuses to run without it.
LOCK_DIR ?= $(CURDIR)/lock

##@ Build

.PHONY: debs
debs: check-host ## Copy the locked DOCA OVS debs for UBUNTU_SERIES and ARCH into OUT_DIR.
	@case "$(OUT_DIR)" in \
	  /*) ;; \
	  "") echo "doca-ovs: OUT_DIR is required, as an absolute path." >&2; exit 1 ;; \
	  *) echo "doca-ovs: OUT_DIR must be an absolute path, got '$(OUT_DIR)'." >&2; exit 1 ;; \
	esac
	@command -v dpkg-scanpackages >/dev/null 2>&1 || \
		{ echo "doca-ovs: dpkg-scanpackages is required to index OUT_DIR; install dpkg-dev." >&2; exit 1; }
	./fetch.sh --doca-version "$(DOCA_VERSION)" --lock-dir "$(LOCK_DIR)" \
		--series "$(UBUNTU_SERIES)" --arch "$(ARCH)" --dist "$(DIST_DIR)" --out "$(OUT_DIR)"
	@# Index everything in OUT_DIR: the other node packages share it.
	@cd "$(OUT_DIR)" && dpkg-scanpackages . /dev/null >Packages
	@echo "wrote $(OUT_DIR)/Packages"

.PHONY: lock
lock: check-host ## Regenerate lock/<version>.lock from the signed DOCA index, for every series and arch. Review the diff, then commit it.
	@command -v gpg >/dev/null 2>&1 && command -v gpgv >/dev/null 2>&1 || \
		{ echo "doca-ovs: gpg and gpgv are required to verify the DOCA index." >&2; exit 1; }
	./fetch.sh --write-lock --doca-version "$(DOCA_VERSION)" --lock-dir "$(LOCK_DIR)"
	@echo
	@echo "Review the lock diff before committing it:"
	@echo "    git diff -- $(LOCK_DIR)"

##@ Checks

.PHONY: check-host
check-host: ## Fail early when this host cannot run the fetch.
	@missing=""; \
	for tool in curl sha256sum; do \
		command -v "$$tool" >/dev/null 2>&1 || missing="$$missing $$tool"; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "doca-ovs: missing tools needed to fetch the DOCA debs:$$missing" >&2; \
		exit 1; \
	fi

.PHONY: clean
clean: ## Remove the download cache.
	rm -rf $(DIST_DIR)
