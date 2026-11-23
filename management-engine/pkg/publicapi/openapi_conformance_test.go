//go:build integration_test

// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

const jsonMedia = "application/json"

// apiConformance checks live requests and responses against a generated OpenAPI document.
type apiConformance struct {
	document             *openapi3.T
	naming               resource.Naming
	server               *httptest.Server
	successfulOperations map[string]bool
	datastoreUnavailable atomic.Bool
	router               routers.Router
}

func newAPIConformance(datastoreConfig *rest.Config, surface Surface) *apiConformance {
	document, err := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "api", surface.Name+".openapi.yaml"))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(document.Validate(context.Background())).To(gomega.Succeed())
	router, err := gorillamux.NewRouter(document)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	mux := http.NewServeMux()
	clientConfig := resource.ClientConfig{Datastore: datastoreConfig, Surface: surface.Name, Naming: surface.Naming}
	if surface.Name == "tenant" {
		clientConfig.Namespace = func(context.Context) (string, error) { return "org-a", nil }
	}
	for _, registration := range surface.Registrations {
		gomega.Expect(registration.Mount(mux, clientConfig)).To(gomega.Succeed())
	}
	api := &apiConformance{document: document, naming: surface.Naming, successfulOperations: map[string]bool{}, router: router}
	MountHealth(mux, func(context.Context) error {
		if api.datastoreUnavailable.Load() {
			return fmt.Errorf("datastore unavailable")
		}
		return nil
	})
	api.server = httptest.NewServer(resource.WithLimits(mux, resource.ServerOptions{Naming: surface.Naming}))
	ginkgo.DeferCleanup(api.server.Close)
	api.server.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return api
}

// checkExchange validates request syntax, response status, response body and ETag presence.
// schemaValid describes OpenAPI validation, which does not enforce every runtime rule.
func (api *apiConformance) checkExchange(
	ctx context.Context,
	want int,
	method string,
	target string,
	headers http.Header,
	body any,
	schemaValid bool,
) (http.Header, map[string]any) {
	ginkgo.GinkgoHelper()
	var bodyBytes []byte
	switch content := body.(type) {
	case nil:
	case []byte:
		bodyBytes = content
	default:
		var err error
		bodyBytes, err = json.Marshal(content)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	headers = headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	if bodyBytes != nil && headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", jsonMedia)
	}
	snapshot, err := api.sendRequest(ctx, fixtureRequest{Method: method, Path: target, Headers: headers, Body: bodyBytes})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	label := fmt.Sprintf("%s %s -> %d %#v", method, target, snapshot.StatusCode, snapshot.Body)
	gomega.Expect(snapshot.StatusCode).To(gomega.Equal(want), label)
	if schemaValid {
		gomega.Expect(snapshot.requestError).NotTo(gomega.HaveOccurred(), label)
	} else {
		gomega.Expect(snapshot.requestError).To(gomega.HaveOccurred(), label)
	}
	decoded, _ := snapshot.Body.(map[string]any)
	return snapshot.Headers, decoded
}

// validateExchange checks the actual response and reports whether OpenAPI accepts the request.
func (api *apiConformance) validateExchange(request *http.Request, response *http.Response, body []byte) error {
	ginkgo.GinkgoHelper()
	route, pathParams, err := api.router.FindRoute(request)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "%s %s", request.Method, request.URL.Path)
	operation := route.Operation

	// Validation can fill defaults, so it must use its own copy of the request body.
	validationRequest := request.Clone(request.Context())
	if request.GetBody != nil {
		var err error
		validationRequest.Body, err = request.GetBody()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	validationInput := &openapi3filter.RequestValidationInput{
		Request: validationRequest, PathParams: pathParams, Route: route,
		Options: &openapi3filter.Options{MultiError: true, IncludeResponseStatus: true},
	}
	validationErr := openapi3filter.ValidateRequest(request.Context(), validationInput)
	label := fmt.Sprintf("%s %s -> %d %s", request.Method, request.URL.Path, response.StatusCode, body)
	validationOutput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: validationInput, Status: response.StatusCode,
		Header: response.Header, Options: validationInput.Options,
	}
	validationOutput.SetBodyBytes(body)
	gomega.Expect(openapi3filter.ValidateResponse(request.Context(), validationOutput)).To(gomega.Succeed(), label)
	documented := operation.Responses.Status(response.StatusCode)
	gomega.Expect(documented).NotTo(gomega.BeNil(), label)
	if documented.Value.Headers["ETag"] != nil {
		gomega.Expect(response.Header.Get("ETag")).NotTo(gomega.BeEmpty(), label)
	} else {
		gomega.Expect(response.Header.Get("ETag")).To(gomega.BeEmpty(), label)
	}
	if response.StatusCode < http.StatusMultipleChoices {
		gomega.Expect(validationErr).NotTo(gomega.HaveOccurred(), label)
		api.successfulOperations[operation.OperationID] = true
	}
	return validationErr
}

func resourceBody(name string, spec map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "spec": spec}
}

// exerciseResource checks CRUDL success, validation failures and write preconditions.
func (api *apiConformance) exerciseResource(ctx context.Context, collection string, spec map[string]any) {
	ginkgo.GinkgoHelper()
	const schemaValid, schemaInvalid = true, false
	item := func(name string) string { return collection + "/" + name }
	plainTextHeaders := http.Header{"Content-Type": {"text/plain"}}
	oversized := append(bytes.Repeat([]byte(" "), resource.DefaultMaxBodyBytes), `{}`...)
	create := api.document.Paths.Value(collection).Post.RequestBody.Value.Content[jsonMedia].Schema.Value
	required := create.Properties["spec"].Value.Required
	incomplete := maps.Clone(spec)
	delete(incomplete, required[0])
	// The service decodes strictly, so the document must forbid what it does not declare, at any depth.
	withStatus, withUnknown := resourceBody("third", spec), resourceBody("third", spec)
	withStatus["status"], withUnknown["unknown"] = map[string]any{}, true
	nested := maps.Clone(spec)
	nested["unknown"] = true

	createdHeaders, _ := api.checkExchange(ctx, 201, http.MethodPost, collection, nil,
		resourceBody("first", spec), schemaValid)
	api.checkExchange(ctx, 201, http.MethodPost, collection, nil, resourceBody("second", spec), schemaValid)
	api.checkExchange(ctx, 409, http.MethodPost, collection, nil, resourceBody("first", spec), schemaValid)
	// The document and the service must agree on which spec fields are required: each one is missed,
	// and together they are enough.
	minimal := map[string]any{}
	for _, field := range required {
		without := maps.Clone(spec)
		delete(without, field)
		api.checkExchange(ctx, 422, http.MethodPost, collection, nil, resourceBody("third", without), schemaInvalid)
		minimal[field] = spec[field]
	}
	minimalHeaders, _ := api.checkExchange(ctx, 201, http.MethodPost, collection, nil,
		resourceBody("minimal", minimal), schemaValid)
	api.checkExchange(ctx, 202, http.MethodDelete, item("minimal"),
		http.Header{"If-Match": {minimalHeaders.Get("ETag")}}, nil, schemaValid)
	api.checkExchange(ctx, 400, http.MethodPost, collection, nil, withStatus, schemaInvalid)
	api.checkExchange(ctx, 400, http.MethodPost, collection, nil, withUnknown, schemaInvalid)
	api.checkExchange(ctx, 400, http.MethodPost, collection, nil, resourceBody("third", nested), schemaInvalid)
	api.checkExchange(ctx, 415, http.MethodPost, collection, plainTextHeaders, []byte(`{}`), schemaInvalid)
	api.checkExchange(ctx, 413, http.MethodPost, collection, nil, oversized, schemaInvalid)

	api.checkExchange(ctx, 200, http.MethodGet, item("first"), nil, nil, schemaValid)
	api.checkExchange(ctx, 404, http.MethodGet, item("missing"), nil, nil, schemaValid)
	api.checkExchange(ctx, 400, http.MethodGet, item("Not_A_Name"), nil, nil, schemaInvalid)
	api.checkExchange(ctx, 200, http.MethodGet, collection, nil, nil, schemaValid)
	api.checkExchange(ctx, 400, http.MethodGet, collection+"?unknown=1", nil, nil, schemaValid)
	api.checkExchange(ctx, 400, http.MethodGet, collection+"?page_size=501", nil, nil, schemaInvalid)
	_, page := api.checkExchange(ctx, 200, http.MethodGet, collection+"?page_size=1", nil, nil, schemaValid)
	gomega.Expect(page["has_more"]).To(gomega.BeTrue())
	next := collection + "?page_size=1&page_key=" + url.QueryEscape(page["page_key"].(string))
	api.checkExchange(ctx, 200, http.MethodGet, next, nil, nil, schemaValid)

	staleHeaders := http.Header{"If-Match": {`"1"`}}
	currentHeaders := http.Header{"If-Match": {createdHeaders.Get("ETag")}}
	updatedHeaders, _ := api.checkExchange(ctx, 200, http.MethodPut, item("first"), currentHeaders,
		resourceBody("first", spec), schemaValid)
	currentHeaders = http.Header{"If-Match": {updatedHeaders.Get("ETag")}}
	api.checkExchange(ctx, 412, http.MethodPut, item("first"), staleHeaders, resourceBody("first", spec), schemaValid)
	api.checkExchange(ctx, 428, http.MethodPut, item("first"), nil, resourceBody("first", spec), schemaInvalid)
	api.checkExchange(ctx, 404, http.MethodPut, item("missing"), staleHeaders, resourceBody("missing", spec), schemaValid)
	api.checkExchange(ctx, 400, http.MethodPut, item("first"), currentHeaders, resourceBody("second", spec), schemaValid)
	api.checkExchange(ctx, 422, http.MethodPut, item("first"), currentHeaders,
		resourceBody("first", incomplete), schemaInvalid)
	api.checkExchange(ctx, 415, http.MethodPut, item("first"), plainTextHeaders, []byte(`{}`), schemaInvalid)
	api.checkExchange(ctx, 413, http.MethodPut, item("first"), currentHeaders, oversized, schemaInvalid)

	api.checkExchange(ctx, 428, http.MethodDelete, item("first"), nil, nil, schemaInvalid)
	api.checkExchange(ctx, 412, http.MethodDelete, item("first"), staleHeaders, nil, schemaValid)
	api.checkExchange(ctx, 400, http.MethodDelete, item("first")+"?propagation_policy=orphan",
		currentHeaders, nil, schemaInvalid)
	api.checkExchange(ctx, 404, http.MethodDelete, item("missing"), staleHeaders, nil, schemaValid)
	api.checkExchange(ctx, 202, http.MethodDelete, item("first")+"?propagation_policy=background",
		currentHeaders, nil, schemaValid)
	second, _ := api.checkExchange(ctx, 200, http.MethodGet, item("second"), nil, nil, schemaValid)
	api.checkExchange(ctx, 202, http.MethodDelete, item("second"),
		http.Header{"If-Match": {second.Get("ETag")}}, nil, schemaValid)
	api.checkExchange(ctx, 404, http.MethodGet, item("second"), nil, nil, schemaValid)
}

// checkAllOperations requires a successful exchange for every documented operation.
func (api *apiConformance) checkAllOperations() {
	ginkgo.GinkgoHelper()
	for _, pathItem := range api.document.Paths.Map() {
		for _, operation := range pathItem.Operations() {
			gomega.Expect(api.successfulOperations[operation.OperationID]).To(gomega.BeTrue(), operation.OperationID)
		}
	}
}

// The committed documents must describe what the APIs really accept and return, for every operation.
var _ = ginkgo.Describe("OpenAPI documents", func() {
	ginkgo.It("describe every operation the APIs serve", func(ctx ginkgo.SpecContext) {
		environment := &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true, UseExistingCluster: new(false),
		}
		datastoreConfig, err := environment.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(environment.Stop)
		datastore, err := client.New(datastoreConfig, client.Options{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		namespace := &corev1.Namespace{Name: "org-a"}
		gomega.Expect(datastore.Create(ctx, namespace)).To(gomega.Succeed())
		operator := newAPIConformance(datastoreConfig, Operator())
		tenant := newAPIConformance(datastoreConfig, Tenant())

		// The catalog fixture and the cluster sample are valid by their own tests, so their specs are reused,
		// keyed by the name each object has there.
		canonicalSpecs := map[string]map[string]any{}
		specs := map[string]map[string]any{}
		specTypes := map[string]reflect.Type{}
		for _, registration := range append(Operator().Registrations, Tenant().Registrations...) {
			description := registration.Describe()
			field, found := description.SourceType.FieldByName("Spec")
			gomega.Expect(found).To(gomega.BeTrue())
			specTypes[description.Info.GVK.Kind] = field.Type
		}
		for _, file := range []string{
			filepath.Join("testdata", "catalog", "tenant-catalog-read-views", "input_client_objects.yaml"),
			filepath.Join("..", "..", "config", "samples", "service_v1_nvkecluster.yaml"),
		} {
			bodyBytes, err := os.ReadFile(file)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(bodyBytes), 4096)
			for {
				object := &unstructured.Unstructured{}
				err := decoder.Decode(&object.Object)
				if err == io.EOF {
					break
				}
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				if object.Object != nil {
					spec, found, err := unstructured.NestedMap(object.Object, "spec")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(found).To(gomega.BeTrue(), "%s has no spec", object.GetName())
					canonicalSpecs[object.GetName()] = spec
					naming := operator.naming
					if object.GetKind() == "NVKECluster" {
						naming = tenant.naming
					}
					specs[object.GetName()] = fixtureWireValue(specTypes[object.GetKind()], spec, naming).(map[string]any)
				}
			}
		}
		const (
			images   = "/v1/machine-images"
			releases = "/v1/cluster-releases"
			channels = "/v1/cluster-release-channels"
			classes  = "/v1/control-plane-classes"
			profiles = "/v1/node-profiles"
		)
		catalog := map[string]string{
			images: "img-ubuntu", releases: "cr-old", channels: "stable", classes: "standard", profiles: "np-b34",
		}
		for collection, name := range catalog {
			ginkgo.By("operator " + collection)
			operator.exerciseResource(ctx, collection, specs[name])
		}
		ginkgo.By("tenant /v1/clusters")
		tenant.exerciseResource(ctx, "/v1/clusters", specs["prod-us-west"])
		tenant.exerciseNaming(ctx, datastore, canonicalSpecs["prod-us-west"])

		ginkgo.By("JWT audience uniqueness")
		for _, test := range []struct {
			name        string
			audiences   []any
			status      int
			schemaValid bool
		}{
			{"unique-audiences", []any{"first", "second"}, http.StatusCreated, true},
			// The datastore's apply rejection currently maps to the generic 503 response.
			{"duplicate-audiences", []any{"first", "first"}, http.StatusServiceUnavailable, false},
		} {
			cluster := runtime.DeepCopyJSON(specs["prod-us-west"])
			cluster["authentication"] = map[string]any{"jwt": []any{map[string]any{
				"issuer":         map[string]any{"url": "https://issuer.example.invalid", "audiences": test.audiences},
				"claim_mappings": map[string]any{"username": map[string]any{"claim": "sub"}},
			}}}
			tenant.checkExchange(ctx, test.status, http.MethodPost, "/v1/clusters", nil,
				resourceBody(test.name, cluster), test.schemaValid)
		}

		ginkgo.By("publishing the catalog")
		for collection, name := range catalog {
			operator.checkExchange(ctx, 201, http.MethodPost, collection, nil, resourceBody(name, specs[name]), true)
		}

		ginkgo.By("an update that changes a field another writer owns")
		otherWriterIntent := &unstructured.Unstructured{
			Object: map[string]any{"spec": runtime.DeepCopyJSON(canonicalSpecs["standard"])},
		}
		otherWriterIntent.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("ControlPlaneClass"))
		otherWriterIntent.SetName("standard")
		gomega.Expect(unstructured.SetNestedField(otherWriterIntent.Object, int64(5), "spec", "controlPlane", "replicas")).
			To(gomega.Succeed())
		gomega.Expect(datastore.Apply(ctx, client.ApplyConfigurationFromUnstructured(otherWriterIntent),
			client.FieldOwner("another-writer"), client.ForceOwnership)).
			To(gomega.Succeed())
		changed, _ := operator.checkExchange(ctx, 200, http.MethodGet, classes+"/standard", nil, nil, true)
		operator.checkExchange(ctx, 409, http.MethodPut, classes+"/standard", http.Header{"If-Match": {changed.Get("ETag")}},
			resourceBody("standard", specs["standard"]), true)

		ginkgo.By("health probes")
		for _, api := range []*apiConformance{operator, tenant} {
			api.checkExchange(ctx, 200, http.MethodGet, LivenessPath, nil, nil, true)
			api.checkExchange(ctx, 200, http.MethodGet, ReadinessPath, nil, nil, true)
			api.datastoreUnavailable.Store(true)
			api.checkExchange(ctx, 200, http.MethodGet, LivenessPath, nil, nil, true)
			api.checkExchange(ctx, 503, http.MethodGet, ReadinessPath, nil, nil, true)
			api.datastoreUnavailable.Store(false)
		}

		ginkgo.By("tenant catalog views")
		views := []string{classes, channels, profiles}
		for _, view := range views {
			name := catalog[view]
			_, listed := tenant.checkExchange(ctx, 200, http.MethodGet, view, nil, nil, true)
			gomega.Expect(listed["items"]).NotTo(gomega.BeEmpty(), view)
			tenant.checkExchange(ctx, 200, http.MethodGet, view+"/"+name, nil, nil, true)
			tenant.checkExchange(ctx, 404, http.MethodGet, view+"/missing", nil, nil, true)
			tenant.checkExchange(ctx, 400, http.MethodGet, view+"/Not_A_Name", nil, nil, false)
			tenant.checkExchange(ctx, 400, http.MethodGet, view+"?unknown=1", nil, nil, true)
			tenant.checkExchange(ctx, 400, http.MethodGet, view+"?page_size=501", nil, nil, false)
		}
		tenant.checkExchange(ctx, 200, http.MethodGet, channels+"?version=1.34", nil, nil, true)
		tenant.checkExchange(ctx, 200, http.MethodGet, channels+"/stable?version=1.34", nil, nil, true)
		tenant.checkExchange(ctx, 400, http.MethodGet, channels+"?version=not-a-version", nil, nil, false)
		tenant.checkExchange(ctx, 400, http.MethodGet, channels+"/stable?version=not-a-version", nil, nil, false)
		tenant.checkExchange(ctx, 200, http.MethodGet, profiles+"?field_selector=spec.profile%3Dubuntu-gpu", nil, nil, true)
		tenant.checkExchange(ctx, 400, http.MethodGet, profiles+"?field_selector=spec.unknown%3Dx", nil, nil, true)

		ginkgo.By("a page key that outlives the catalog it was issued for")
		for _, view := range views {
			operator.checkExchange(ctx, 201, http.MethodPost, view, nil, resourceBody("extra", specs[catalog[view]]), true)
			_, page := tenant.checkExchange(ctx, 200, http.MethodGet, view+"?page_size=1", nil, nil, true)
			gomega.Expect(page["has_more"]).To(gomega.BeTrue(), view)
			operator.checkExchange(ctx, 201, http.MethodPost, view, nil, resourceBody("newer", specs[catalog[view]]), true)
			next := view + "?page_size=1&page_key=" + url.QueryEscape(page["page_key"].(string))
			tenant.checkExchange(ctx, 410, http.MethodGet, next, nil, nil, true)
		}

		operator.checkAllOperations()
		tenant.checkAllOperations()
	})
})
