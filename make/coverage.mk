# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Where a module writes its coverage profile, and in what mode.
#
# Three collected coverage before this file existed, and nothing read any of it:
# controller/Makefile wrote coverage.txt beside the module and said so in a
# comment, nico-sim wrote its own, management-engine wrote two more, and the
# other six that have tests ran `go test` with no -coverprofile at all. A
# profile nobody reads is close to no profile, but no profile at all is worse --
# there is then nothing to notice when a refactor drops a test file or a build
# tag quietly excludes a suite, because the run still passes.
#
# This file is the single owner of the answer, the way make/tool-versions.mk
# owns the tool pins. A module includes it and appends $(GO_COVER_FLAGS) to its
# own `go test` line; the paths, the mode and the directory are not restated
# anywhere else. hack/coverage.sh reads the same directory, so neither side
# holds a second copy of the layout.
#
# READ docs/coverage-policy.md BEFORE ATTACHING A NUMBER TO ANY OF THIS. A
# percentage is a measure of which lines a test binary executed. It is not a
# measure of whether anything was asserted about them, so it is not evidence of
# test adequacy, and this repository does not gate on one.

# Derived from this file's own location, because each module includes it from a
# different depth -- same technique as make/golangci.mk. `:=` matters: by the
# time a recipe runs, MAKEFILE_LIST has grown past this file.
COVERAGE_MK_DIR := $(dir $(lastword $(MAKEFILE_LIST)))
COVERAGE_ROOT   := $(abspath $(COVERAGE_MK_DIR)..)

# One directory for every module's profile, at the repository root, so a CI job
# can upload `coverage/` without naming eleven paths and a developer can run
# `go tool cover -html` against any of them. Gitignored.
COVERAGE_DIR ?= $(COVERAGE_ROOT)/coverage

# The module's path relative to the repository root, with the separators
# flattened: controller/nico-sim becomes controller-nico-sim. A module whose
# Makefile is not at its module root sets COVERAGE_NAME itself.
COVERAGE_MODULE ?= $(patsubst $(COVERAGE_ROOT)/%,%,$(CURDIR))
COVERAGE_NAME   ?= $(subst /,-,$(COVERAGE_MODULE))

# atomic, not set or count: the merge gate runs these suites with -race, and
# `go test` refuses -covermode=count with -race. atomic is correct under both,
# and uniform means two modules' profiles can be concatenated.
GO_COVER_MODE ?= atomic

# coverprofile_name <suffix> -- the file name, and coverprofile <suffix> -- the
# full path. The suffix exists for management-engine, the one module whose API
# suite and envtest suite are separate `go test` invocations that cannot share
# an output file.
#
# A DOT separates the suffix, not a dash. Dash is what COVERAGE_NAME already
# uses for the path separators, so with a dash hack/coverage.sh could not tell
# `controller` plus a suffix from the module `controller-nico-sim`: globbing
# controller's profiles as `controller-*.out` would sweep up nico-sim's,
# charts' and test/e2e's and report their statements as the controller's. A dot
# cannot appear in a flattened module name, so the two namespaces stay apart.
coverprofile_name = $(COVERAGE_NAME)$(if $(1),.$(1)).out
coverprofile      = $(COVERAGE_DIR)/$(call coverprofile_name,$(1))

COVERPROFILE   ?= $(call coverprofile,)
GO_COVER_FLAGS ?= -coverprofile=$(COVERPROFILE) -covermode=$(GO_COVER_MODE)

# `go test` will not create the directory for -coverprofile; it fails with
# "no such file or directory" after running the whole suite, which is an
# expensive way to find out. Every test recipe that writes a profile runs this
# first.
COVERAGE_MKDIR = mkdir -p $(COVERAGE_DIR)
