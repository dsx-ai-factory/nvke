// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

const (
	otlpEndpointFlag = "otlp-endpoint"
	otlpInsecureFlag = "otlp-insecure"
)

// otlpSource reads one source of OTLP configuration. ok is false when the
// source names no endpoint. strict is true when an error should fail the
// command, because the operator asked for the export explicitly.
type otlpSource func(cmd *cobra.Command) (cfg telemetry.OTLPConfig, ok, strict bool, err error)

// otlpSources are the places OTLP configuration comes from, in order of
// precedence: the first that names an endpoint wins.
var otlpSources = []otlpSource{otlpFromFlags, otlpFromEnv} //nolint:gochecknoglobals // ordered lookup table

// resolveOTLP returns the OTLP configuration for the command about to run, from
// the first source that names an endpoint. strict is true when that endpoint,
// or an error about it, came from a flag, so the caller fails the command
// instead of continuing without export.
func resolveOTLP(cmd *cobra.Command) (cfg telemetry.OTLPConfig, ok, strict bool, err error) {
	for _, source := range otlpSources {
		cfg, ok, strict, err = source(cmd)
		if ok || err != nil {
			return cfg, ok, strict, err
		}
	}
	return telemetry.OTLPConfig{}, false, false, nil
}

// otlpFromFlags reads --otlp-endpoint and --otlp-insecure, which only
// "bootstrap site" registers. The endpoint must be reachable from the temporary
// controller in the bootstrap cluster, so a loopback address is rejected.
func otlpFromFlags(cmd *cobra.Command) (telemetry.OTLPConfig, bool, bool, error) {
	endpointFlag := cmd.Flags().Lookup(otlpEndpointFlag)
	if endpointFlag == nil {
		return telemetry.OTLPConfig{}, false, false, nil
	}
	endpoint := endpointFlag.Value.String()
	insecure, _ := cmd.Flags().GetBool(otlpInsecureFlag)
	if endpoint == "" {
		// No endpoint from the flags, whether the flag was left out or given empty.
		if cmd.Flags().Changed(otlpInsecureFlag) {
			return telemetry.OTLPConfig{}, false, true, errors.New("--" + otlpInsecureFlag + " applies to --" + otlpEndpointFlag + "; pass --" +
				otlpEndpointFlag + " too, or drop --" + otlpInsecureFlag)
		}
		return telemetry.OTLPConfig{}, false, false, nil
	}
	parsed, err := telemetry.ParseEndpoint(endpoint)
	if err == nil {
		err = telemetry.RejectLoopbackEndpoint(endpoint, parsed)
	}
	if err != nil {
		return telemetry.OTLPConfig{}, false, true, fmt.Errorf("--%s: %w", otlpEndpointFlag, err)
	}
	// An https endpoint always uses TLS: the flag would be ignored, not obeyed.
	if parsed.HTTPS && insecure {
		return telemetry.OTLPConfig{}, false, true, errors.New("--" + otlpInsecureFlag + " conflicts with the https scheme of --" + otlpEndpointFlag +
			"; use an http:// endpoint or drop the scheme, or drop --" + otlpInsecureFlag)
	}
	cfg := telemetry.OTLPConfig{Endpoint: parsed.Host, Insecure: parsed.Insecure || insecure, Headers: telemetry.HeadersFromEnv()}
	return cfg, true, true, nil
}

// otlpFromEnv reads the standard OTEL_EXPORTER_OTLP_* variables. A problem with
// them never fails the command.
func otlpFromEnv(*cobra.Command) (telemetry.OTLPConfig, bool, bool, error) {
	cfg, ok, err := telemetry.OTLPConfigFromEnv()
	return cfg, ok, false, err
}
