//go:build integration_test

// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"io"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
)

var _ = fixture.DescribeCaseSet(func() fixture.CaseSet {
	set := publicAPICaseSet("Tenant catalog", "tenant-catalog-", "tenant", Tenant,
		func() []client.ObjectList { return nil })
	set.Setup = seedCatalogObjects
	return set
}())

// seedCatalogObjects covers internal metadata the operator API cannot write.
// Strict unstructured admission catches fields missing from the CRD schema.
func seedCatalogObjects(ctx ginkgo.SpecContext, tc *fixture.Case, _ fixture.CaseSet) {
	data, exists := tc.Input("input_client_objects.yaml")
	if !exists {
		return
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(data), 4096)
	for {
		object := &unstructured.Unstructured{}
		err := decoder.Decode(&object.Object)
		if err == io.EOF {
			break
		}
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		if object.Object == nil {
			continue
		}
		gomega.Expect(tc.Client.Create(ctx, object, client.FieldValidation("Strict"))).To(gomega.Succeed(),
			"catalog fixture %s/%s must be admitted unchanged by the production CRD", object.GetKind(), object.GetName())
	}
}
