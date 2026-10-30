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
	"net/http/httptest"
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
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

func TestOperatorAPI(t *testing.T) {
	t.Setenv("USE_EXISTING_CLUSTER", "false")
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Operator API Suite")
}

type fixtureRequest struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Headers http.Header     `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
}

type fixtureResponse struct {
	StatusCode int         `json:"statusCode"`
	Headers    http.Header `json:"headers"`
	Body       any         `json:"body"`
}

var _ = fixture.DescribeCaseSet(fixture.CaseSet{
	Description:          "Operator API",
	DirPrefix:            "operator-",
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
	CompareObjects: func() []client.ObjectList {
		return []client.ObjectList{&v1.MachineImageList{}, &v1.ClusterReleaseList{}}
	},
	Setup: func(ctx ginkgo.SpecContext, tc *fixture.Case, _ fixture.CaseSet) {
		if tc.HasInput("input_client_objects.yaml") {
			gomega.Expect(tc.CreateObjects(ctx)).To(gomega.Succeed())
		}
	},
	DefineSteps: func(tc *fixture.Case, _ fixture.CaseSet) {
		ginkgo.It("serves the fixture request", func(ctx ginkgo.SpecContext) {
			mux := http.NewServeMux()
			for _, registration := range Operator() {
				gomega.Expect(registration.Mount(mux, resource.ClientConfig{
					Datastore: tc.Config,
					Surface:   "operator",
				})).To(gomega.Succeed())
			}
			server := httptest.NewServer(resource.WithLimits(mux, resource.ServerOptions{}))
			ginkgo.DeferCleanup(server.Close)
			server.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

			if data, exists := tc.Input("input_setup.yaml"); exists {
				var setup []fixtureRequest
				gomega.Expect(yaml.UnmarshalStrict([]byte(data), &setup)).To(gomega.Succeed())
				for _, input := range setup {
					_, err := sendRequest(ctx, server, input)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}

			data, exists := tc.Input("input_request.yaml")
			gomega.Expect(exists).To(gomega.BeTrue())
			var input fixtureRequest
			gomega.Expect(yaml.UnmarshalStrict([]byte(data), &input)).To(gomega.Succeed())
			snapshot, err := sendRequest(ctx, server, input)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			tc.AddGolden("expected_response.yaml", func(context.Context) (string, error) {
				for name, value := range map[string]string{"ETag": `"RESOURCE_VERSION"`, "Date": "DATE"} {
					values := snapshot.Headers.Values(name)
					for i := range values {
						values[i] = value
					}
				}
				body, _ := snapshot.Body.(map[string]any)
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
					if metadata["creationTimestamp"] != nil {
						metadata["creationTimestamp"] = "1970-01-01T00:00:00Z"
					}
					if metadata["deletionTimestamp"] != nil {
						metadata["deletionTimestamp"] = "1970-01-01T00:00:00Z"
					}
				}
				data, err := yaml.Marshal(snapshot)
				return string(data), err
			})
		})
	},
})

func sendRequest(ctx context.Context, server *httptest.Server, input fixtureRequest) (fixtureResponse, error) {
	var snapshot fixtureResponse
	request, err := http.NewRequestWithContext(ctx, input.Method, server.URL+input.Path, bytes.NewReader(input.Body))
	if err != nil {
		return snapshot, fmt.Errorf("create request: %w", err)
	}
	functions := template.FuncMap{
		"etag": func() (string, error) {
			path, _, _ := strings.Cut(input.Path, "?")
			current, err := sendRequest(ctx, server, fixtureRequest{Method: http.MethodGet, Path: path})
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
	response, err := server.Client().Do(request)
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
	snapshot.StatusCode, snapshot.Headers = response.StatusCode, response.Header.Clone()
	if len(data) != 0 {
		if err := json.Unmarshal(data, &snapshot.Body); err != nil {
			snapshot.Body = string(data)
		}
	}
	return snapshot, nil
}
