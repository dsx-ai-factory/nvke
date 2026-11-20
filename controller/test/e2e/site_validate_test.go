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

// The validation image's management cluster checks: the site lane's
// post-deploy validation, run against the cluster the suite is pointed at
// rather than one it built.
const (
	// siteKubernetesVersionEnv is the Kubernetes version the management
	// cluster was built with, overriding managementCluster.kubernetesVersion
	// for one run. Unset in both fails the version check rather than
	// skipping it.
	siteKubernetesVersionEnv = "NVKE_MGMT_CLUSTER_KUBERNETES_VERSION"
	// siteWorkloadVersionsEnv overrides managementCluster.workloadVersions
	// for one run, as namespace/workload=version comma-separated.
	siteWorkloadVersionsEnv = "NVKE_MGMT_CLUSTER_WORKLOAD_VERSIONS"
	// fieldScope is how much of the cluster the health checks hold to:
	// cluster, the default, or namespaces, for a management cluster shared
	// with workloads nvke neither installs nor depends on.
	fieldScope = "managementCluster.scope"
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
	// account on a management cluster, and the provisioned ServiceAccount on a tenant.
	checkIdentity     bool
	kubernetesVersion resolvedVersion
	checks            siteChecks
	expect            siteExpectations
}

// siteValidateFromProfile reads the settings from the profile's
// managementCluster section. The probe pod defaults to the suite's own
// namespace, read from namespaceFile, so that a runner needs to grant pod
// rights in no namespace but the one it already runs in; outside a pod it
// falls back to default. The pod check excuses only that own namespace, and
// none outside a pod, so a probe namespace given or defaulted never hides
// another namespace's failing pods.
func siteValidateFromProfile(p profileManagementCluster, getenv func(string) string, namespaceFile string) (siteValidateSettings, error) {
	enabled, err := parseRequireEnv(siteValidateEnv, getenv(siteValidateEnv))
	if err != nil {
		return siteValidateSettings{}, err
	}

	controllerNamespace := cmp.Or(getenv(controller.EnvNamespace), controller.ServiceNamespace)
	namespaces := siteNamespaces(controllerNamespace)
	if p.Namespaces != nil {
		namespaces, err = profileNamespaces(p.Namespaces, sectionManagementCluster+".namespaces")
		if err != nil {
			return siteValidateSettings{}, err
		}
	}

	ownNamespace := ""
	if own, readErr := os.ReadFile(namespaceFile); readErr == nil {
		ownNamespace = strings.TrimSpace(string(own))
	}
	probeNamespace := cmp.Or(strings.TrimSpace(p.Probe.Namespace), ownNamespace, "default")

	expect, err := siteExpectationsFromProfile(p.profileExpectations, sectionManagementCluster, siteWorkloadVersionsEnv, getenv)
	if err != nil {
		return siteValidateSettings{}, err
	}
	scoped, err := scopedToNamespaces(p.Scope)
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
		kubernetesVersion: resolveVersion(sectionManagementCluster+".kubernetesVersion", p.KubernetesVersion, siteKubernetesVersionEnv, getenv),
		checks: siteChecks{
			namespaces:       namespaces,
			probeNamespace:   probeNamespace,
			probeImage:       profileProbeImage(p.Probe.profileProbe),
			excusedNamespace: ownNamespace,
			certManager:      true,
			scope:            scope,
		},
		expect: expect,
	}, nil
}

// profileNamespaces reads a namespaces field, which replaces the default
// list and so must name at least one.
func profileNamespaces(list []string, field string) ([]siteNamespace, error) {
	names := trimList(list)
	namespaces := make([]siteNamespace, 0, len(names))
	for _, name := range names {
		namespaces = append(namespaces, siteNamespace{name: name, source: field})
	}
	if len(namespaces) == 0 {
		return nil, fmt.Errorf("%s names no namespace", field)
	}
	return namespaces, nil
}

// profileProbeImage is the probe pods' image, which needs sh and nslookup,
// and the Secrets in the probe namespace they pull it with: they run as the
// namespace's default service account, not the runner's.
func profileProbeImage(p profileProbe) probeImage {
	image := probeImage{ref: cmp.Or(strings.TrimSpace(p.Image), kube.KubectlImage)}
	for _, name := range trimList(p.PullSecrets) {
		image.pullSecrets = append(image.pullSecrets, corev1.LocalObjectReference{Name: name})
	}
	return image
}

// scopedToNamespaces reads fieldScope: whether the checks hold to the
// management cluster's namespaces rather than the whole cluster.
func scopedToNamespaces(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", siteScopeCluster:
		return false, nil
	case siteScopeNamespaces:
		return true, nil
	}
	return false, fmt.Errorf("%s: want %s or %s, got %q", fieldScope, siteScopeCluster, siteScopeNamespaces, raw)
}

// newSiteScope is the required namespaces and the declared components' own,
// reusing the lists a management cluster already gives rather than a second one to keep in
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
	lines := append(runNotes(), s.kubernetesVersion.String())
	lines = append(lines, s.checks.describe()...)
	return append(lines, s.expect.describe()...)
}

// Every check runs whatever the others found, unlike the site lane's: there
// is no machine to release here, and a report naming every problem is the
// point. Hence ContinueOnFailure, without which an Ordered container skips
// every spec after the first that fails.
var _ = Describe("management cluster validation", Ordered, ContinueOnFailure, Label(siteValidateLabel), func() {
	settings, err := siteValidateFromProfile(validateProfile.ManagementCluster, os.Getenv, serviceAccountNamespaceFile)
	resolve := func() siteValidateSettings {
		Expect(err).NotTo(HaveOccurred(), "management cluster validation configuration")
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
			AddReportEntry("management cluster validation inputs", strings.Join(settings.describe(), "\n"))
		}
	})

	validateSitePostDeploy(func() (site.Cluster, site.Spec, bool) {
		resolve()
		return site.Cluster{}, site.Spec{KubernetesVersion: settings.kubernetesVersion.value}, true
	}, settings.checks)
	validateSiteBehavior(resolve)
})
