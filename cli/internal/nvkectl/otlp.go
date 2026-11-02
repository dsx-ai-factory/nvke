// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

const (
	otlpEndpointFlag = "otlp-endpoint"
	otlpInsecureFlag = "otlp-insecure"

	// bootstrapExportAnnotation marks "bootstrap site", whose --otlp-endpoint and
	// --otlp-insecure flags say where its own telemetry goes. "config set-site"
	// has flags of the same names, which only name an endpoint to store.
	bootstrapExportAnnotation = "nvkectl/bootstrap-export"
)

// siteCommands are the top-level commands that load a site, so their telemetry
// goes to that site's OTLP endpoint. A command such as `create ... --dry-run=client`
// exports there although it may contact nothing, because the lookup cannot see
// flag values. So does `get bootstrap-manifest`, which is under "get".
var siteCommands = []string{"create", "delete", "deprecate", "get", "patch"} //nolint:gochecknoglobals // constant lookup table

// otlpSource reads one source of OTLP configuration. ok is false when the
// source names no endpoint. strict is true when an error should fail the
// command, because the operator asked for the export explicitly.
type otlpSource func(cmd *cobra.Command) (cfg telemetry.OTLPConfig, ok, strict bool, err error)

// otlpSources are the places OTLP configuration comes from, in order of
// precedence: the first that names an endpoint wins.
var otlpSources = []otlpSource{otlpFromFlags, otlpFromSite, otlpFromEnv} //nolint:gochecknoglobals // ordered lookup table

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

// otlpFromFlags reads --otlp-endpoint and --otlp-insecure on "bootstrap site".
// The endpoint must be reachable from the temporary controller in the bootstrap
// cluster, so a loopback address is rejected.
func otlpFromFlags(cmd *cobra.Command) (telemetry.OTLPConfig, bool, bool, error) {
	if cmd.Annotations[bootstrapExportAnnotation] != "true" {
		return telemetry.OTLPConfig{}, false, false, nil
	}
	endpoint, _ := cmd.Flags().GetString(otlpEndpointFlag)
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

// otlpFromSite reads the OTLP endpoint of the site a command uses: the one
// --site names, else the current site. Only the commands in siteCommands have
// one. A config that cannot be read or a site that is missing gives no
// endpoint and no error: the command reports that itself, and export is best
// effort. A bad endpoint is not strict, because it was configured earlier and
// not typed on this command line. Headers come from the environment, as the
// config never stores them.
func otlpFromSite(cmd *cobra.Command) (telemetry.OTLPConfig, bool, bool, error) {
	if !slices.Contains(siteCommands, topLevel(cmd)) {
		return telemetry.OTLPConfig{}, false, false, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return telemetry.OTLPConfig{}, false, false, nil
	}
	siteCfg, err := resolveSiteConfig(cfg)
	if err != nil || siteCfg.OTLPEndpoint == "" {
		return telemetry.OTLPConfig{}, false, false, nil
	}
	parsed, err := telemetry.ParseEndpoint(siteCfg.OTLPEndpoint)
	// An https endpoint always uses TLS, as set-site enforces; a hand-edited config
	// could still pair it with otlp-insecure and send the headers in clear text.
	if err == nil && parsed.HTTPS && siteCfg.OTLPInsecure {
		err = errors.New("otlp-insecure conflicts with the https scheme of the endpoint")
	}
	if err != nil {
		name := site
		if name == "" {
			name = cfg.CurrentSite
		}
		return telemetry.OTLPConfig{}, false, false, &siteEndpointError{site: name, err: err}
	}
	return telemetry.OTLPConfig{
		Endpoint: parsed.Host,
		Insecure: parsed.Insecure || siteCfg.OTLPInsecure,
		Headers:  telemetry.HeadersFromEnv(),
	}, true, false, nil
}

// siteEndpointError is a stored site endpoint that cannot be used. It carries
// the site name so the warning can name the source of the bad value.
type siteEndpointError struct {
	site string
	err  error
}

func (e *siteEndpointError) Error() string { return "site OTLP endpoint: " + e.err.Error() }
func (e *siteEndpointError) Unwrap() error { return e.err }

// otlpFromEnv reads the standard OTEL_EXPORTER_OTLP_* variables. A problem with
// them never fails the command.
func otlpFromEnv(*cobra.Command) (telemetry.OTLPConfig, bool, bool, error) {
	cfg, ok, err := telemetry.OTLPConfigFromEnv()
	return cfg, ok, false, err
}
