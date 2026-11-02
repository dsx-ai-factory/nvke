// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"cmp"
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

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
	// siteProbePullSecretsEnv lists, comma-separated, the Secrets in the probe
	// namespace that the probe pods pull their image with: they run as the
	// namespace's default service account, not the runner's.
	siteProbePullSecretsEnv = "NVKE_SITE_PROBE_PULL_SECRETS"
)

// serviceAccountNamespaceFile names the namespace a pod runs in.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// siteValidateSettings is the image's site validation configuration.
type siteValidateSettings struct {
	enabled bool
	// checkIdentity holds the suite's identity to read access. It is set
	// where that identity is one the README grants: the image's service
	// account on a site, and the provisioned ServiceAccount on a tenant.
	checkIdentity     bool
	kubernetesVersion string
	checks            siteChecks
	expect            siteExpectations
}

// siteValidateFromEnv reads the settings. The probe pod defaults to the
// suite's own namespace, read from namespaceFile, so that a runner needs to
// grant pod rights in no namespace but the one it already runs in; outside a
// pod it falls back to default. The pod check excuses only that own
// namespace, and none outside a pod, so a probe namespace given or defaulted
// never hides another namespace's failing pods.
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

	ownNamespace := ""
	if own, readErr := os.ReadFile(namespaceFile); readErr == nil {
		ownNamespace = strings.TrimSpace(string(own))
	}
	probeNamespace := cmp.Or(strings.TrimSpace(getenv(siteProbeNamespaceEnv)), ownNamespace)
	if probeNamespace == "" {
		probeNamespace = "default"
	}

	image := probeImage{ref: cmp.Or(strings.TrimSpace(getenv(siteProbeImageEnv)), kube.KubectlImage)}
	for _, name := range splitList(getenv(siteProbePullSecretsEnv), ",") {
		image.pullSecrets = append(image.pullSecrets, corev1.LocalObjectReference{Name: name})
	}

	expect, err := siteExpectationsFromEnv(getenv, sitePrefix)
	if err != nil {
		return siteValidateSettings{}, err
	}
	if _, err := parseRequireEnv(keepOnFailureEnv, getenv(keepOnFailureEnv)); err != nil {
		return siteValidateSettings{}, err
	}

	return siteValidateSettings{
		enabled:           enabled,
		checkIdentity:     ownNamespace != "",
		kubernetesVersion: strings.TrimSpace(getenv(siteKubernetesVersionEnv)),
		checks: siteChecks{
			namespaces:       namespaces,
			probeNamespace:   probeNamespace,
			probeImage:       image,
			excusedNamespace: ownNamespace,
			certManager:      true,
		},
		expect: expect,
	}, nil
}

// describe lists the settings the run resolved, for the report.
func (s siteValidateSettings) describe() []string {
	lines := []string{siteKubernetesVersionEnv + "=" + s.kubernetesVersion}
	lines = append(lines, s.checks.describe()...)
	return append(lines, s.expect.describe()...)
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
	BeforeAll(func() {
		if err == nil && settings.enabled {
			AddReportEntry("site validation inputs", strings.Join(settings.describe(), "\n"))
		}
	})

	validateSitePostDeploy(func() (site.Cluster, site.Spec, bool) {
		resolve()
		return site.Cluster{}, site.Spec{KubernetesVersion: settings.kubernetesVersion}, true
	}, settings.checks)
	validateSiteBehavior(resolve)
})
