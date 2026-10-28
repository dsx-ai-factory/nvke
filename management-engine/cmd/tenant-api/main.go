// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
	"github.com/dsx-ai-factory/nvke/management-engine/public"
)

var version = "dev"

func main() {
	namespace := "tenant-test"
	flag.Func("namespace",
		"Temporary PoC namespace; replace with authenticated caller scope (default \"tenant-test\")",
		func(value string) error {
			if len(validation.IsDNS1123Label(value)) != 0 {
				return fmt.Errorf("must be a valid Kubernetes namespace")
			}
			namespace = value
			return nil
		})
	cfg := resource.ClientConfig{
		Surface: "tenant",
		// Remove the PoC namespace flag and resolve the authenticated caller's namespace once auth is wired.
		Namespace: func(context.Context) (string, error) {
			return namespace, nil
		},
	}
	if err := public.Run(version, public.Tenant(), cfg); err != nil {
		slog.Error("tenant API stopped", "error", err)
		os.Exit(1)
	}
}
