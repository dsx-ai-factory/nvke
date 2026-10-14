// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestInspectSiteSpec(t *testing.T) {
	spec := filledSiteSpec(t, nil)
	stdout, _, err := runGenerate(t, "inspect", "sitespec", spec)
	if err != nil {
		t.Fatal(err)
	}
	want := "SITE_NAME='my-tier0-site'\nSITE_VIP='10.10.50.10'\nSITE_TRUST_DOMAIN='my-tier0-site.nke.local'\nSITE_ARCH='arm64'\n"
	if stdout != want {
		t.Errorf("got:\n%s\nwant:\n%s", stdout, want)
	}
	if _, _, err := runGenerate(t, "inspect", "sitespec", "/nonexistent/sitespec.yaml"); err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("missing file: %v", err)
	}
}
