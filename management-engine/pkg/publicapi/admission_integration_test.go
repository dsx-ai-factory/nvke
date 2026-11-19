//go:build integration_test

// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

var _ = fixture.DescribeCaseSet(func() fixture.CaseSet {
	set := publicAPICaseSet("Operator admission", "admission-", "operator", Operator,
		func() []client.ObjectList { return []client.ObjectList{&v1.NodeProfileList{}} })
	set.Setup = installAdmissionPolicies
	return set
}())

// installAdmissionPolicies installs the admission policies from config/admission/policies, then the fixture objects.
// The policy reads existing profiles from a cache, so setup waits until it denies a copy of each fixture profile.
func installAdmissionPolicies(ctx ginkgo.SpecContext, tc *fixture.Case, _ fixture.CaseSet) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "admission", "policies", "*.yaml"))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(paths).NotTo(gomega.BeEmpty())
	for _, path := range paths {
		data, err := os.ReadFile(path)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// Each file holds a policy and its binding.
		decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			object := &unstructured.Unstructured{}
			err := decoder.Decode(&object.Object)
			if errors.Is(err, io.EOF) {
				break
			}
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "decode %s", path)
			if len(object.Object) == 0 {
				continue
			}
			gomega.Expect(tc.Client.Create(ctx, object)).To(gomega.Succeed(), "install %s %s", path, object.GetKind())
		}
	}

	gomega.Expect(tc.CreateObjects(ctx)).To(gomega.Succeed())
	var profiles v1.NodeProfileList
	gomega.Expect(tc.Client.List(ctx, &profiles)).To(gomega.Succeed())
	gomega.Expect(profiles.Items).NotTo(gomega.BeEmpty())
	// Wait until the policy denies a copy of every fixture profile, so the requests see them all.
	gomega.Eventually(func(g gomega.Gomega) {
		for _, profile := range profiles.Items {
			duplicate := &v1.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: profile.Name + "-probe"}, Spec: profile.Spec}
			err := tc.Client.Create(ctx, duplicate, client.DryRunAll)
			g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "duplicate of %s admitted: %v", profile.Name, err)
			g.Expect(err.Error()).To(gomega.ContainSubstring("already published by NodeProfile " + profile.Name))
		}
	}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(gomega.Succeed())
}
