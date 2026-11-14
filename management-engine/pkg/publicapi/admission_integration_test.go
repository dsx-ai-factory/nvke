//go:build integration_test

// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"os"
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

var _ = fixture.DescribeCaseSet(func() fixture.CaseSet {
	set := publicAPICaseSet("Operator admission", "admission-", "operator", Operator,
		func() []client.ObjectList { return []client.ObjectList{&v1.NodePoolReleaseList{}} })
	set.Setup = installAdmissionPolicies
	return set
}())

// installAdmissionPolicies installs the production admission policies from config/admission.
// The API server enforces a new policy only after it maps the policy's parameter kind,
// so setup waits until a duplicate node profile is denied before the fixture requests run.
func installAdmissionPolicies(ctx ginkgo.SpecContext, tc *fixture.Case, _ fixture.CaseSet) {
	for _, name := range []string{"validatingadmissionpolicy.yaml", "validatingadmissionpolicybinding.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "admission", name))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		object := &unstructured.Unstructured{}
		gomega.Expect(yaml.Unmarshal(data, &object.Object)).To(gomega.Succeed())
		gomega.Expect(tc.Client.Create(ctx, object)).To(gomega.Succeed(), "install %s", name)
	}

	probe := func(name string) *v1.NodePoolRelease {
		return &v1.NodePoolRelease{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1.NodePoolReleaseSpec{
				MachineImageRef: v1.MachineImageReference{Name: "admission-probe"},
				MachineInit:     v1.MachineInit{UserDataTemplate: "#cloud-config\n"},
				Profile:         "admission-probe",
				Tag:             "ready",
			},
		}
	}
	gomega.Eventually(func(g gomega.Gomega) {
		g.Expect(client.IgnoreAlreadyExists(tc.Client.Create(ctx, probe("admission-probe")))).To(gomega.Succeed())
		err := tc.Client.Create(ctx, probe("admission-probe-duplicate"), client.DryRunAll)
		g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "duplicate node profile admitted: %v", err)
		g.Expect(err.Error()).To(gomega.ContainSubstring("already published by NodePoolRelease admission-probe"))
	}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(gomega.Succeed())
	gomega.Expect(tc.Client.Delete(ctx, probe("admission-probe"))).To(gomega.Succeed())
}
