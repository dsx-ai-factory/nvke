// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"flag"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var cfgPath string

func init() {
	flag.StringVar(&cfgPath, "e2e.config", "e2e-config.yaml", "path to the E2E config file (missing file -> all defaults)")
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NKE E2E Suite")
}
