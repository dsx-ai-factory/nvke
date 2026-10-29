// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controller names where the controller deployment under test lives.
// One definition, so a second spelling cannot drift and quietly point the suite
// somewhere else.
package controller

import (
	"cmp"
	"os"
)

// The defaults; read the first three through Namespace, Service and Port.
// DeploymentName is also the name of the controller container.
const (
	ServiceNamespace = "nvke-controller"
	ServiceName      = "nvke-controller"
	ServicePort      = "8080"
	DeploymentName   = "nvke-controller"
)

// The overrides, which apply suite-wide so moving the controller moves
// everything at once. The Legacy names are the deprecated spellings (the
// NKE_CONTROLLER_ ones, then the CWE_ ones), still read when the new name is
// unset. #1399 removes them.
const (
	EnvNamespace = "NVKE_CONTROLLER_NAMESPACE"
	EnvService   = "NVKE_CONTROLLER_SERVICE"
	EnvPort      = "NVKE_CONTROLLER_SERVICE_PORT"

	PreviousEnvNamespace = "NKE_CONTROLLER_NAMESPACE"
	PreviousEnvService   = "NKE_CONTROLLER_SERVICE"
	PreviousEnvPort      = "NKE_CONTROLLER_SERVICE_PORT"

	LegacyEnvNamespace = "CWE_NAMESPACE"
	LegacyEnvService   = "CWE_SERVICE"
	LegacyEnvPort      = "CWE_SERVICE_PORT"
)

// Namespace is the namespace to look for the controller in.
func Namespace() string {
	return cmp.Or(os.Getenv(EnvNamespace), os.Getenv(PreviousEnvNamespace), os.Getenv(LegacyEnvNamespace), ServiceNamespace)
}

// Service is the Service in front of the controller pods.
func Service() string {
	return cmp.Or(os.Getenv(EnvService), os.Getenv(PreviousEnvService), os.Getenv(LegacyEnvService), ServiceName)
}

// Port is the Service port the controller API is published on, named or numeric.
func Port() string {
	return cmp.Or(os.Getenv(EnvPort), os.Getenv(PreviousEnvPort), os.Getenv(LegacyEnvPort), ServicePort)
}

// Deployment is the controller Deployment, which is also the name of its container.
// The chart names the Deployment, its container and the Service alike, so
// EnvService moves it too.
func Deployment() string {
	return cmp.Or(os.Getenv(EnvService), os.Getenv(PreviousEnvService), os.Getenv(LegacyEnvService), DeploymentName)
}

// ServiceAccount is the ServiceAccount the controller runs as, and so the last segment of
// its SPIFFE ID. Named like the Service; EnvService moves it too.
func ServiceAccount() string {
	return cmp.Or(os.Getenv(EnvService), os.Getenv(PreviousEnvService), os.Getenv(LegacyEnvService), ServiceName)
}
