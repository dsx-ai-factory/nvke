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

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
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
	// siteScopeEnv is how much of the cluster the health checks hold to:
	// cluster, the default, or namespaces, for a site shared with workloads
	// nvke neither installs nor depends on. See siteScopeFromEnv.
	siteScopeEnv = "NVKE_SITE_SCOPE"
)

const (
	siteScopeCluster    = "cluster"
	siteScopeNamespaces = "namespaces"
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

	controllerNamespace := cmp.Or(getenv(controller.EnvNamespace), controller.ServiceNamespace)
	namespaces := siteNamespaces(controllerNamespace)
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
	scoped, err := scopedToNamespaces(getenv(siteScopeEnv))
	if err != nil {
		return siteValidateSettings{}, err
	}
	var scope siteScope
	if scoped {
		scope = newSiteScope(namespaces, expect, controllerNamespace)
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
			scope:            scope,
		},
		expect: expect,
	}, nil
}

// scopedToNamespaces reads siteScopeEnv: whether the checks hold to the
// site's namespaces rather than the whole cluster.
func scopedToNamespaces(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", siteScopeCluster:
		return false, nil
	case siteScopeNamespaces:
		return true, nil
	}
	return false, fmt.Errorf("%s=%q: want %s or %s", siteScopeEnv, raw, siteScopeCluster, siteScopeNamespaces)
}

// newSiteScope is the required namespaces and the declared components' own,
// reusing the lists a site already gives rather than a second one to keep in
// step. The checks a broken neighbor can still break nvke through, nodes,
// the API server and admission webhooks among them, stay cluster-wide.
// kube-system and the controller's namespace are always in it: a list that
// replaces the default one and leaves them out would otherwise move the CNI,
// CoreDNS and the controller itself outside what fails a check.
func newSiteScope(namespaces []siteNamespace, expect siteExpectations, controllerNamespace string) siteScope {
	scope := siteScope{kubeSystemNamespace: true, controllerNamespace: true}
	for _, ns := range namespaces {
		scope[ns.name] = true
	}
	for _, ns := range expect.components {
		scope[ns] = true
	}
	return scope
}

// describe lists the settings the run resolved, for the report.
func (s siteValidateSettings) describe() []string {
	lines := append(runNotes(), siteKubernetesVersionEnv+"="+s.kubernetesVersion)
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
