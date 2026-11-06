// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	kubeadmv1beta3 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
)

// ExtraArgs holds the operator's flags for each control-plane component, as a
// map from flag name without the leading "--" to its value.
type ExtraArgs struct {
	APIServer         map[string]string
	ControllerManager map[string]string
	Scheduler         map[string]string
}

// component names a control-plane component as kubeadm's ClusterConfiguration
// and the NVKE controller API do.
type component string

const (
	componentAPIServer         component = "apiServer"
	componentControllerManager component = "controllerManager"
	componentScheduler         component = "scheduler"
)

// nvkeSetFlags lists every flag the renderer can set on a component, whether or
// not it sets it for a given cluster: in clusterConfiguration and in the
// kubeadm-config rewrites in configmap.go. An operator argument for one of them
// is rejected, because NVKE's security settings and its own rollouts depend on
// them. feature-gates is not listed: operator gates are merged with NVKE's.
// TestNVKESetFlagsMatchRenderer fails when the renderer and this list disagree.
func nvkeSetFlags(c component) []string {
	switch c {
	case componentAPIServer:
		return []string{
			"enable-admission-plugins",
			"audit-policy-file",
			"audit-log-path",
			"audit-log-maxage",
			"audit-log-maxbackup",
			"audit-log-maxsize",
			"kubelet-certificate-authority",
			etcdCompactionIntervalFlagName,
			profilingFlag,
			bindAddressFlag,
			"service-account-extend-token-expiration",
			"tls-cipher-suites",
			"shutdown-delay-duration",
			"shutdown-send-retry-after",
			"shutdown-watch-termination-grace-period",
			encryptionProviderConfigFlagName,
			encryptionProviderConfigReloadFlagName,
			AuthConfigFlagName,
		}
	case componentControllerManager:
		return []string{profilingFlag, bindAddressFlag, "secondary-node-eviction-rate", "large-cluster-size-threshold", "terminated-pod-gc-threshold"}
	case componentScheduler:
		return []string{profilingFlag, bindAddressFlag}
	default:
		return nil
	}
}

var (
	// flagNameRE accepts only lowercase words joined by single hyphens.
	// Components parse flags with pflag's WordSepNormalizeFunc, which turns "_"
	// into "-", so "enable_admission_plugins" would otherwise pass the
	// nvkeSetFlags check and still override enable-admission-plugins. It also
	// rejects a leading "--", which would render as "----name".
	flagNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

	featureGateNameRE = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

// ValidateExtraArgs reports the first operator argument that would not render
// for kubernetesVersion: a malformed flag name, a flag NVKE sets, a
// feature-gates value that doesn't parse, or a gate that contradicts NVKE's.
func ValidateExtraArgs(kubernetesVersion string, args ExtraArgs) error {
	var cc kubeadmv1beta3.ClusterConfiguration
	return applyExtraArgs(&cc, kubernetesVersion, args)
}

// applyExtraArgs merges the operator's arguments into each component's
// extraArgs in cc, after the flags NVKE set there.
func applyExtraArgs(cc *kubeadmv1beta3.ClusterConfiguration, kubernetesVersion string, args ExtraArgs) error {
	targets := []struct {
		component component
		extraArgs *map[string]string
		operator  map[string]string
	}{
		{componentAPIServer, &cc.APIServer.ExtraArgs, args.APIServer},
		{componentControllerManager, &cc.ControllerManager.ExtraArgs, args.ControllerManager},
		{componentScheduler, &cc.Scheduler.ExtraArgs, args.Scheduler},
	}
	for _, t := range targets {
		merged, err := mergeComponentArgs(t.component, kubernetesVersion, *t.extraArgs, t.operator)
		if err != nil {
			return fmt.Errorf("%s.extraArgs: %w", t.component, err)
		}
		*t.extraArgs = merged
	}
	return nil
}

// mergeComponentArgs returns base with the operator's flags added. base is
// returned unchanged when there are none, so a cluster without arguments
// renders exactly as before.
func mergeComponentArgs(c component, kubernetesVersion string, base, operator map[string]string) (map[string]string, error) {
	if len(operator) == 0 {
		return base, nil
	}
	merged := maps.Clone(base)
	if merged == nil {
		merged = make(map[string]string, len(operator))
	}
	// Sorted, so a request with several bad flags always reports the same one.
	for _, name := range slices.Sorted(maps.Keys(operator)) {
		value := operator[name]
		if !flagNameRE.MatchString(name) {
			return nil, fmt.Errorf("flag name %q must be lowercase words joined by hyphens, without the leading --", name)
		}
		if name == featureGatesFlag {
			gates, err := mergeFeatureGates(c, kubernetesVersion, value)
			if err != nil {
				return nil, err
			}
			merged[name] = gates
			continue
		}
		if slices.Contains(nvkeSetFlags(c), name) {
			return nil, fmt.Errorf("flag %q is set by NVKE and can't be overridden", name)
		}
		merged[name] = value
	}
	return merged, nil
}

// mergeFeatureGates parses the operator's feature-gates value, merges it with
// the gates NVKE sets on the component and renders the result sorted by name.
func mergeFeatureGates(c component, kubernetesVersion, value string) (string, error) {
	gates, err := parseFeatureGates(value)
	if err != nil {
		return "", err
	}
	for name, enabled := range nvkeFeatureGates(c, kubernetesVersion) {
		if operatorEnabled, ok := gates[name]; ok && operatorEnabled != enabled {
			return "", fmt.Errorf("feature gate %q contradicts NVKE, which sets it to %t on Kubernetes %s", name, enabled, kubernetesVersion)
		}
		gates[name] = enabled
	}
	return formatFeatureGates(gates), nil
}

// parseFeatureGates parses a comma-separated list of Name=true or Name=false.
func parseFeatureGates(value string) (map[string]bool, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("feature-gates is empty")
	}
	gates := map[string]bool{}
	for entry := range strings.SplitSeq(value, ",") {
		name, setting, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || !featureGateNameRE.MatchString(name) || (setting != "true" && setting != "false") {
			return nil, fmt.Errorf("feature-gates entry %q is not Name=true or Name=false", strings.TrimSpace(entry))
		}
		// AllAlpha and AllBeta set every gate of a stage, including gates a
		// later Kubernetes version adds, so an upgrade would turn on features
		// nobody chose. Gates have to be named one by one.
		if name == "AllAlpha" || name == "AllBeta" {
			return nil, fmt.Errorf("feature gate %q sets every gate of a stage and isn't accepted; name the gates you need", name)
		}
		if _, seen := gates[name]; seen {
			return nil, fmt.Errorf("feature gate %q is set more than once", name)
		}
		gates[name] = setting == "true"
	}
	return gates, nil
}

func formatFeatureGates(gates map[string]bool) string {
	entries := make([]string, 0, len(gates))
	for _, name := range slices.Sorted(maps.Keys(gates)) {
		entries = append(entries, name+"="+strconv.FormatBool(gates[name]))
	}
	return strings.Join(entries, ",")
}

// nvkeFeatureGates returns the gates NVKE sets on a component for
// kubernetesVersion: today only ImageVolume, on kube-apiserver, below its GA.
func nvkeFeatureGates(c component, kubernetesVersion string) map[string]bool {
	if c != componentAPIServer || !needsImageVolumeFeatureGate(kubernetesVersion) {
		return nil
	}
	return map[string]bool{imageVolumeFeatureGate: true}
}
