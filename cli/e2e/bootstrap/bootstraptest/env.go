// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// The environment the bootstrap lanes configure a run through. run.sh and
// run-nico.sh already own these values; passing them in the environment is
// what lets the lane keep deciding them.
const (
	// EnvSiteSpec names the site spec to bootstrap from. It is the only
	// required variable.
	EnvSiteSpec = "NVKECTL_E2E_SITESPEC"
	// EnvBootstrapLog names the file the command's combined output is written
	// to. The lane greps it afterwards, so it is how the shell assertions
	// survive unchanged.
	EnvBootstrapLog = "NVKECTL_E2E_BOOTSTRAP_LOG"
	// EnvKubeconfigOut names where the finished cluster's kubeconfig goes.
	EnvKubeconfigOut = "NVKECTL_E2E_KUBECONFIG_OUT"
	// EnvEncryptionKeyOut names where the etcd encryption key goes.
	EnvEncryptionKeyOut = "NVKECTL_E2E_ENCRYPTION_KEY_OUT"
	// EnvDiagnosticsDir names where a failed bootstrap dumps cluster state.
	EnvDiagnosticsDir = "NVKECTL_E2E_DIAGNOSTICS_DIR"
	// EnvTimeout is the command's own --timeout, as a Go duration.
	EnvTimeout = "NVKECTL_E2E_BOOTSTRAP_TIMEOUT"
	// EnvKeepBootstrapCluster leaves the ephemeral Kind cluster standing. The
	// lane's teardown already owns removing it.
	EnvKeepBootstrapCluster = "NVKECTL_E2E_KEEP_BOOTSTRAP_CLUSTER"
	// EnvDisableMTLS stands the site up without SPIRE.
	EnvDisableMTLS = "NVKECTL_E2E_DISABLE_MTLS"
	// EnvVerbose passes -v, so a successful run writes its diagnostics bundle.
	EnvVerbose = "NVKECTL_E2E_VERBOSE"
)

// BootstrapOptionsFromEnv reads a run from the environment, resolving the
// nvkectl binary under cliDir. It reports an error when a required variable is
// missing and when one holds a value that cannot be parsed, rather than
// falling back to a default that would test something other than what the lane
// asked for.
func BootstrapOptionsFromEnv(cliDir string) (BootstrapOptions, error) {
	var opts BootstrapOptions

	binary, err := FindNvkectl(cliDir)
	if err != nil {
		return opts, err
	}

	siteSpec := os.Getenv(EnvSiteSpec)
	if siteSpec == "" {
		return opts, fmt.Errorf("%s is not set: name the site spec to bootstrap from", EnvSiteSpec)
	}

	timeout, err := durationEnv(EnvTimeout)
	if err != nil {
		return opts, err
	}
	keep, err := boolEnv(EnvKeepBootstrapCluster)
	if err != nil {
		return opts, err
	}
	disableMTLS, err := boolEnv(EnvDisableMTLS)
	if err != nil {
		return opts, err
	}
	verbose, err := boolEnv(EnvVerbose)
	if err != nil {
		return opts, err
	}

	opts = BootstrapOptions{
		Binary:               binary,
		SiteSpec:             siteSpec,
		KubeconfigOut:        os.Getenv(EnvKubeconfigOut),
		EncryptionKeyOut:     os.Getenv(EnvEncryptionKeyOut),
		DiagnosticsDir:       os.Getenv(EnvDiagnosticsDir),
		Timeout:              timeout,
		KeepBootstrapCluster: keep,
		DisableMTLS:          disableMTLS,
		Verbose:              verbose,
		LogFile:              os.Getenv(EnvBootstrapLog),
	}
	return opts, nil
}

// durationEnv parses a Go duration from the named variable. An unset or empty
// variable is a zero duration and no error.
func durationEnv(name string) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a duration (for example 15m): %w", name, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s=%q is negative", name, raw)
	}
	return d, nil
}

// boolEnv parses a bool from the named variable, accepting what
// strconv.ParseBool accepts. An unset or empty variable is false and no error.
func boolEnv(name string) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a boolean (for example 1, true, false): %w", name, raw, err)
	}
	return v, nil
}
