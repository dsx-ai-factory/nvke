// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cwe names where the NKE deployment under test lives. One definition,
// so a second spelling cannot drift and quietly point the suite somewhere else.
package cwe

import (
	"cmp"
	"os"
)

// The defaults; read the first three through Namespace, Service and Port.
// DeploymentName is also the name of the NKE container.
const (
	ServiceNamespace = "cloud-workflow-engine"
	ServiceName      = "cloud-workflow-engine"
	ServicePort      = "8080"
	DeploymentName   = "cloud-workflow-engine"
)

// The overrides, which apply suite-wide so moving NKE moves everything at once.
const (
	EnvNamespace = "CWE_NAMESPACE"
	EnvService   = "CWE_SERVICE"
	EnvPort      = "CWE_SERVICE_PORT"
)

// Namespace is the namespace to look for NKE in.
func Namespace() string { return cmp.Or(os.Getenv(EnvNamespace), ServiceNamespace) }

// Service is the Service in front of the NKE pods.
func Service() string { return cmp.Or(os.Getenv(EnvService), ServiceName) }

// Port is the Service port the NKE API is published on, named or numeric.
func Port() string { return cmp.Or(os.Getenv(EnvPort), ServicePort) }
