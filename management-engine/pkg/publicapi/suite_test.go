//go:build integration_test

// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-api-provider-nico/pkg/test/fixture"
	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// TestPublicAPI runs the operator and tenant API fixture suites against envtest.
func TestPublicAPI(t *testing.T) {
	t.Setenv("USE_EXISTING_CLUSTER", "false")
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Public API Suite")
}

type fixtureRequest struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Headers http.Header     `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
}

type fixtureResponse struct {
	StatusCode   int         `json:"statusCode"`
	Headers      http.Header `json:"headers"`
	Body         any         `json:"body"`
	requestError error
}

var _ = fixture.DescribeCaseSet(publicAPICaseSet("Operator API", "operator-", Operator(),
	func() []client.ObjectList {
		return []client.ObjectList{&v1.MachineImageList{}, &v1.ClusterReleaseList{}}
	}))

// publicAPICaseSet configures isolated API fixtures with optional operator setup requests
// and golden comparisons for a single request or a request batch.
func publicAPICaseSet(
	description, prefix string,
	surface Surface,
	compareObjects func() []client.ObjectList,
) fixture.CaseSet {
	return fixture.CaseSet{
		Description:          description,
		DirPrefix:            prefix,
		MaskExpectedMetadata: true,
		SchemeFn: func() *runtime.Scheme {
			scheme := runtime.NewScheme()
			gomega.Expect(v1.AddToScheme(scheme)).To(gomega.Succeed())
			return scheme
		},
		EnvironmentFn: func(*fixture.Case) *envtest.Environment {
			return &envtest.Environment{
				CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
				ErrorIfCRDPathMissing: true,
				UseExistingCluster:    new(false),
			}
		},
		CompareObjects: compareObjects,
		Setup: func(ctx ginkgo.SpecContext, tc *fixture.Case, _ fixture.CaseSet) {
			if tc.HasInput("input_client_objects.yaml") {
				gomega.Expect(tc.CreateObjects(ctx)).To(gomega.Succeed())
			}
		},
		DefineSteps: func(tc *fixture.Case, _ fixture.CaseSet) {
			ginkgo.It("serves the fixture request", func(ctx ginkgo.SpecContext) {
				api := newAPIConformance(tc.Config, surface)

				if data, exists := tc.Input("input_setup.yaml"); exists {
					setupAPI := api
					if surface.Name == "tenant" {
						setupAPI = newAPIConformance(tc.Config, Operator())
					}
					var setup []fixtureRequest
					gomega.Expect(yaml.UnmarshalStrict([]byte(data), &setup)).To(gomega.Succeed())
					for _, input := range setup {
						response, err := setupAPI.sendRequest(ctx, input)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(response.StatusCode).To(gomega.BeNumerically(">=", http.StatusOK),
							"setup %s %s", input.Method, input.Path)
						gomega.Expect(response.StatusCode).To(gomega.BeNumerically("<", http.StatusMultipleChoices),
							"setup %s %s: %#v", input.Method, input.Path, response.Body)
					}
				}

				if data, exists := tc.Input("input_requests.yaml"); exists {
					gomega.Expect(tc.HasInput("input_request.yaml")).To(gomega.BeFalse(), "choose a single request or a request batch")
					var inputs []fixtureRequest
					gomega.Expect(yaml.UnmarshalStrict([]byte(data), &inputs)).To(gomega.Succeed())
					gomega.Expect(inputs).NotTo(gomega.BeEmpty())
					snapshots := make([]fixtureResponse, 0, len(inputs))
					for _, input := range inputs {
						snapshot, err := api.sendRequest(ctx, input)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						normalizeFixtureResponse(&snapshot)
						snapshots = append(snapshots, snapshot)
					}
					tc.AddGolden("expected_responses.yaml", func(context.Context) (string, error) {
						data, err := yaml.Marshal(snapshots)
						return string(data), err
					})
					return
				}

				data, exists := tc.Input("input_request.yaml")
				gomega.Expect(exists).To(gomega.BeTrue())
				var input fixtureRequest
				gomega.Expect(yaml.UnmarshalStrict([]byte(data), &input)).To(gomega.Succeed())
				snapshot, err := api.sendRequest(ctx, input)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				tc.AddGolden("expected_response.yaml", func(context.Context) (string, error) {
					normalizeFixtureResponse(&snapshot)
					data, err := yaml.Marshal(snapshot)
					return string(data), err
				})
			})
		},
	}
}

// normalizeFixtureResponse replaces volatile values in place for stable snake_case goldens.
// It omits Content-Length when masking a nonempty pagination token.
func normalizeFixtureResponse(snapshot *fixtureResponse) {
	for name, value := range map[string]string{"ETag": `"RESOURCE_VERSION"`, "Date": "DATE"} {
		values := snapshot.Headers.Values(name)
		for i := range values {
			values[i] = value
		}
	}
	body, _ := snapshot.Body.(map[string]any)
	if pageKey, ok := body["page_key"].(string); ok && pageKey != "" {
		body["page_key"] = "PAGE_KEY"
		snapshot.Headers.Del("Content-Length")
	}
	items, _ := body["items"].([]any)
	for _, item := range append([]any{body}, items...) {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		metadata, ok := object["metadata"].(map[string]any)
		if !ok {
			continue
		}
		if metadata["uid"] != nil {
			metadata["uid"] = "UID"
		}
		if metadata["creation_timestamp"] != nil {
			metadata["creation_timestamp"] = "1970-01-01T00:00:00Z"
		}
		if metadata["deletion_timestamp"] != nil {
			metadata["deletion_timestamp"] = "1970-01-01T00:00:00Z"
		}
	}
}

// sendRequest validates a live exchange and captures its response for golden comparison.
// Header templates can resolve the target resource's current ETag.
func (api *apiConformance) sendRequest(ctx context.Context, input fixtureRequest) (fixtureResponse, error) {
	var snapshot fixtureResponse
	request, err := http.NewRequestWithContext(ctx, input.Method, api.server.URL+input.Path, bytes.NewReader(input.Body))
	if err != nil {
		return snapshot, fmt.Errorf("create request: %w", err)
	}
	functions := template.FuncMap{
		"etag": func() (string, error) {
			path, _, _ := strings.Cut(input.Path, "?")
			current, err := api.sendRequest(ctx, fixtureRequest{Method: http.MethodGet, Path: path})
			if err != nil {
				return "", fmt.Errorf("resolve ETag: %w", err)
			}
			return current.Headers.Get("ETag"), nil
		},
	}
	for name, values := range input.Headers {
		for _, value := range values {
			header, err := template.New(name).Funcs(functions).Parse(value)
			if err != nil {
				return snapshot, fmt.Errorf("parse header %s: %w", name, err)
			}
			var rendered bytes.Buffer
			if err := header.Execute(&rendered, nil); err != nil {
				return snapshot, fmt.Errorf("render header %s: %w", name, err)
			}
			request.Header.Add(name, rendered.String())
		}
	}
	response, err := api.server.Client().Do(request)
	if err != nil {
		return snapshot, fmt.Errorf("send request: %w", err)
	}
	data, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return snapshot, fmt.Errorf("read response: %w", readErr)
	}
	if closeErr != nil {
		return snapshot, fmt.Errorf("close response: %w", closeErr)
	}
	snapshot.requestError = api.validateExchange(request, response, data)
	snapshot.StatusCode, snapshot.Headers = response.StatusCode, response.Header.Clone()
	if len(data) != 0 {
		if err := json.Unmarshal(data, &snapshot.Body); err != nil {
			snapshot.Body = string(data)
		}
	}
	return snapshot, nil
}
