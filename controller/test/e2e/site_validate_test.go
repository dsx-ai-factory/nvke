// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/site"
)

// The validation image's site checks: the site lane's post-deploy validation,
// run against the cluster the suite is pointed at rather than one it built.
const (
	// siteKubernetesVersionEnv is the Kubernetes version the site was built
	// with. Unset fails the version check rather than skipping it.
	siteKubernetesVersionEnv = "NVKE_SITE_KUBERNETES_VERSION"
	// siteNamespacesEnv replaces the namespaces that must each run a
	// workload, comma-separated, for a site whose deploy path is not
	// deploy/nico.
	siteNamespacesEnv = "NVKE_SITE_NAMESPACES"
	// siteProbeNamespaceEnv and siteProbeImageEnv place the DNS probe pod and
	// choose its image, which needs sh and nslookup.
	siteProbeNamespaceEnv = "NVKE_SITE_PROBE_NAMESPACE"
	siteProbeImageEnv     = "NVKE_SITE_PROBE_IMAGE"
)

// serviceAccountNamespaceFile names the namespace a pod runs in.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// siteValidateSettings is the image's site validation configuration.
type siteValidateSettings struct {
	enabled           bool
	kubernetesVersion string
	checks            siteChecks
	expect            siteExpectations
}

// siteValidateFromEnv reads the settings. The probe pod defaults to the
// suite's own namespace, read from namespaceFile, so that a runner needs to
// grant pod rights in no namespace but the one it already runs in; outside a
// pod it falls back to default.
func siteValidateFromEnv(getenv func(string) string, namespaceFile string) (siteValidateSettings, error) {
	enabled, err := parseRequireEnv(siteValidateEnv, getenv(siteValidateEnv))
	if err != nil {
		return siteValidateSettings{}, err
	}

	namespaces := siteNamespaces
	if raw := strings.TrimSpace(getenv(siteNamespacesEnv)); raw != "" {
		namespaces = nil
		for name := range strings.SplitSeq(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				namespaces = append(namespaces, siteNamespace{name: name, source: siteNamespacesEnv})
			}
		}
		if len(namespaces) == 0 {
			return siteValidateSettings{}, fmt.Errorf("%s=%q names no namespace", siteNamespacesEnv, raw)
		}
	}

	probeNamespace := strings.TrimSpace(getenv(siteProbeNamespaceEnv))
	if probeNamespace == "" {
		if own, readErr := os.ReadFile(namespaceFile); readErr == nil {
			probeNamespace = strings.TrimSpace(string(own))
		}
	}
	if probeNamespace == "" {
		probeNamespace = "default"
	}

	probeImage := strings.TrimSpace(getenv(siteProbeImageEnv))
	if probeImage == "" {
		probeImage = kube.KubectlImage
	}

	expect, err := siteExpectationsFromEnv(getenv)
	if err != nil {
		return siteValidateSettings{}, err
	}

	return siteValidateSettings{
		enabled:           enabled,
		kubernetesVersion: strings.TrimSpace(getenv(siteKubernetesVersionEnv)),
		checks: siteChecks{
			namespaces:       namespaces,
			probeNamespace:   probeNamespace,
			probeImage:       probeImage,
			excusedNamespace: probeNamespace,
		},
		expect: expect,
	}, nil
}

// Every check runs whatever the others found, unlike the site lane's: there
// is no machine to release here, and a report naming every problem is the
// point. Hence ContinueOnFailure, without which an Ordered container skips
// every spec after the first that fails.
var _ = Describe("site validation", Ordered, ContinueOnFailure, Label("site-validate"), func() {
	settings, err := siteValidateFromEnv(os.Getenv, serviceAccountNamespaceFile)
	resolve := func() siteValidateSettings {
		Expect(err).NotTo(HaveOccurred(), "site validation configuration")
		// siteValidateEnv selects these specs in the image, and gates them
		// everywhere else: a run that selects them by another label
		// validates nothing it did not ask to.
		if !settings.enabled {
			Skip(fmt.Sprintf("%s is not true", siteValidateEnv))
		}
		return settings
	}

	validateSitePostDeploy(func() (site.Cluster, site.Spec, bool) {
		resolve()
		return site.Cluster{}, site.Spec{KubernetesVersion: settings.kubernetesVersion}, true
	}, settings.checks)
	validateSiteBehavior(resolve)
})
