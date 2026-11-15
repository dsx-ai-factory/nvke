// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/publicapi"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

const (
	jsonMediaType = "application/json"
	nameProperty  = "name"
)

const conventions = `Resource requests use JSON. Unknown fields, duplicate JSON keys,
and unsupported or repeated query parameters are rejected.

GET routes also accept HEAD, which returns the same headers without a response body.

These servers do not authenticate callers or enforce permissions per caller.
Tenant and operator servers expose separate sets of routes.
Datastore requests use the server's credentials.

Create, get, and update responses for writable resources include an ETag.
Send it unchanged in If-Match when updating or deleting:
a missing header returns 428 and a stale value returns 412.

PUT replaces the resource configuration managed by this API.
Omitted fields owned by this API are removed or reset to defaults.
DELETE returns 202 when deletion is requested. Cleanup may continue afterward.

Resource errors use application/problem+json with a stable code and optional field details.
Descriptions document additional server-enforced constraints, including immutability and cross-field rules.
Passing OpenAPI schema validation does not guarantee that the server will accept a request.

In writable specs, null object fields are treated as omitted before defaults and validation.
Required fields without defaults still cannot be null.`

var errorDescriptions = map[int]string{
	400: "The request is malformed: invalid JSON, an unknown or read-only field, or an unsupported query parameter.",
	401: "Namespace resolution or datastore authentication failed.",
	403: "The resolved namespace or datastore operation is not permitted.",
	404: "The resource was not found.",
	408: "The request body was not received in time.",
	409: "The resource already exists, or the write conflicts with fields another writer owns.",
	410: "The page key has expired. Start the list again without it.",
	412: "The resource has changed since the version supplied in If-Match.",
	413: "The request body exceeds the size limit.",
	415: "The request body is not application/json.",
	422: "The desired state fails validation. May include diagnostics identifying invalid public fields.",
	428: "The If-Match header is missing.",
	500: "The request could not be completed.",
	503: "The datastore could not complete the request, or a catalog source collection exceeds its read limit.",
}

func document(surface string, registrations []resource.Registration, builder *schemaBuilder) (*openapi3.T, error) {
	overview := map[string]string{
		tenantSurface: "Manage workload clusters in the server-configured namespace and read the global catalog.\n" +
			"Callers cannot select a namespace through the API.",
		operatorSurface: "Publish the machine images, releases, release channels, " +
			"control-plane classes,\nand node profiles used by tenants. These resources are global.",
	}[surface]
	spec := &openapi3.T{
		OpenAPI: "3.0.3",
		Info: &openapi3.Info{
			Title: "NVKE Cluster Service " + surface + " API", Version: "v1",
			Description: overview + "\n\n" + conventions + fmt.Sprintf(
				"\n\nRequest bodies are limited to %d bytes and requests have a %s deadline.\n"+
					"Cancellation is cooperative. A deadline does not guarantee a timeout response.\n"+
					"Responses use Cache-Control: no-store.",
				resource.DefaultMaxBodyBytes, resource.MaxRequestTimeout),
		},
		Extensions: map[string]any{
			"x-max-request-body-bytes":  resource.DefaultMaxBodyBytes,
			"x-request-timeout-seconds": resource.MaxRequestTimeout.Seconds(),
		},
		Servers:    openapi3.Servers{{URL: "/", Description: "The host serving this API."}},
		Paths:      openapi3.NewPaths(),
		Components: &openapi3.Components{Schemas: openapi3.Schemas{}, Responses: openapi3.ResponseBodies{}},
	}
	problem, err := builder.schema(reflect.TypeFor[resource.Error]())
	if err != nil {
		return nil, err
	}
	problem.Value.Description = "A request failure: its HTTP status, a stable code and the fields at fault."
	spec.Components.Schemas["Problem"] = problem
	spec.Components.Responses["Problem"] = problemResponse(
		"Request failure with HTTP status, stable error code and public field diagnostics.")
	for _, registration := range registrations {
		description := registration.Describe()
		if len(description.Routes) == 0 {
			continue
		}
		name := publicName(description)
		if err := resourceSchemas(spec, builder, description, name); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		spec.Tags = append(spec.Tags, &openapi3.Tag{Name: name, Description: "Operations for " + name + " resources."})
		for _, route := range description.Routes {
			if description.ReadOnly && route.Operation != resource.OpGet && route.Operation != resource.OpList {
				return nil, fmt.Errorf("read-only resource %s has %s", name, route.Operation)
			}
			path := spec.Paths.Value(route.Path)
			if path == nil {
				path = &openapi3.PathItem{}
				spec.Paths.Set(route.Path, path)
			}
			if path.GetOperation(route.Method) != nil {
				return nil, fmt.Errorf("duplicate route %s %s", route.Method, route.Path)
			}
			operationSpec, err := operation(description, route, name)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", route.Method, route.Path, err)
			}
			path.SetOperation(route.Method, operationSpec)
			for _, code := range errorStatuses(route.Operation) {
				spec.Components.Responses[strconv.Itoa(code)] = problemResponse(errorDescriptions[code])
			}
		}
	}
	healthOperations(spec)
	return spec, spec.WalkSchemas(describeConstraints)
}

// healthOperations describes the probes publicapi.MountHealth installs beside every surface's resources.
func healthOperations(spec *openapi3.T) {
	const tag = "Health"
	spec.Tags = append(spec.Tags, &openapi3.Tag{
		Name:        tag,
		Description: "Probes for the process that serves this API. They are not resources and take no parameters.",
	})
	liveness := &openapi3.Operation{
		OperationID: "checkLiveness", Summary: "Check liveness", Tags: []string{tag},
		Description: "Reports that the process is serving requests.",
		Responses: openapi3.NewResponses(openapi3.WithStatus(http.StatusOK,
			&openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription("The process is serving.")})),
	}
	readiness := &openapi3.Operation{
		OperationID: "checkReadiness", Summary: "Check readiness", Tags: []string{tag},
		Description: "Reports whether the process can reach the datastore that holds its resources.",
		Responses: openapi3.NewResponses(openapi3.WithStatus(http.StatusOK,
			&openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription("The datastore is reachable.")})),
	}
	readiness.Responses.Set(strconv.Itoa(http.StatusServiceUnavailable), &openapi3.ResponseRef{
		Value: openapi3.NewResponse().WithDescription("The datastore is unreachable.").
			WithContent(content("text/plain", &openapi3.SchemaRef{Value: openapi3.NewStringSchema()})),
	})
	spec.Paths.Set(publicapi.LivenessPath, &openapi3.PathItem{Get: liveness})
	spec.Paths.Set(publicapi.ReadinessPath, &openapi3.PathItem{Get: readiness})
}

// resourceSchemas shares the spec between response and write envelopes, keeping read-only fields out of requests.
func resourceSchemas(spec *openapi3.T, builder *schemaBuilder, description resource.Description, name string) error {
	nameSchema, err := builder.schema(reflect.TypeFor[v1.ResourceName]())
	if err != nil {
		return err
	}
	nameSchema.Value.Description = "Immutable client-selected DNS-subdomain name."
	spec.Components.Schemas["ResourceName"] = nameSchema
	responseSchema, err := builder.schema(description.PublicType)
	if err != nil {
		return err
	}
	responseSchema.Value.Description = "The public representation of a " + name + " resource."
	responseMetadata := responseSchema.Value.Properties["metadata"].Value
	responseMetadata.Description = "The resource's identity and server-assigned metadata."
	responseMetadata.Properties[nameProperty] = schemaRef("ResourceName")
	responseMetadata.Required = []string{nameProperty}
	for field, schema := range responseMetadata.Properties {
		if field != nameProperty {
			schema.Value.ReadOnly = true
			schema.Value.Description = map[string]string{
				"creationTimestamp": "When the resource was created.",
				"deletionTimestamp": "When deletion was requested. Present until cleanup finishes.",
				"uid":               "Distinguishes this resource from an earlier resource with the same name.",
			}[field]
		}
	}
	for _, field := range []string{"spec", "status"} {
		if schema := responseSchema.Value.Properties[field]; schema != nil {
			component := name + strings.ToUpper(field[:1]) + field[1:]
			if field == "status" {
				schema.Value.ReadOnly = true
			} else if !description.ReadOnly {
				allowOptionalNulls(schema.Value)
			}
			if schema.Value.Description == "" {
				// A catalog view composes its spec from several sources, so no single source describes it.
				schema.Value.Description = "The fields of a " + name + " that the catalog publishes."
			}
			spec.Components.Schemas[component] = schema
			responseSchema.Value.Properties[field] = schemaRef(component)
		}
	}
	spec.Components.Schemas[name] = responseSchema
	listSchema, err := builder.schema(description.ListType)
	if err != nil {
		return err
	}
	listSchema.Value.Properties["items"].Value.Items = schemaRef(name)
	listSchema.Value.Description = "One page of " + name + " resources."
	for field, description := range map[string]string{
		"items": "The resources on this page.", "pageKey": "Continuation for the next page. Empty on the last page.",
		"hasMore": "Whether another page follows.",
	} {
		listSchema.Value.Properties[field].Value.Description = description
	}
	spec.Components.Schemas[name+"List"] = listSchema
	if !description.ReadOnly {
		for _, action := range []string{"Create", "Update"} {
			requestMetadata := openapi3.NewObjectSchema().WithPropertyRef(nameProperty, schemaRef("ResourceName")).
				WithRequired([]string{nameProperty}).WithoutAdditionalProperties()
			requestMetadata.Description = "The name of the resource to " + strings.ToLower(action) + "."
			if responseMetadata.Properties["uid"] != nil {
				uid := openapi3.NewStringSchema().WithNullable()
				uid.Description = "Omit or leave empty. A nonempty UID on create is rejected."
				if action == "Update" {
					uid.Description = "Optional identity check. A nonempty UID must match the current resource."
				} else {
					uid.WithMaxLength(0)
				}
				requestMetadata.WithProperty("uid", uid)
			}
			requestSchema := openapi3.NewObjectSchema().WithProperty("metadata", requestMetadata).
				WithPropertyRef("spec", schemaRef(name+"Spec")).
				WithRequired([]string{"metadata", "spec"}).WithoutAdditionalProperties()
			requestSchema.Description = "The body of a request to " + strings.ToLower(action) + " a " + name + " resource."
			spec.Components.Schemas[name+action] = &openapi3.SchemaRef{Value: requestSchema}
		}
	}
	return nil
}

func operation(description resource.Description, route resource.Route, name string) (*openapi3.Operation, error) {
	verb := string(route.Operation)
	operationSpec := &openapi3.Operation{
		OperationID: verb + name, Summary: strings.ToUpper(verb[:1]) + verb[1:] + " " + name,
		Tags: []string{name}, Responses: openapi3.NewResponses(),
	}
	operationSpec.Responses.Set("default", &openapi3.ResponseRef{Ref: "#/components/responses/Problem"})
	for _, code := range errorStatuses(route.Operation) {
		status := strconv.Itoa(code)
		operationSpec.Responses.Set(status, &openapi3.ResponseRef{Ref: "#/components/responses/" + status})
	}
	if strings.HasSuffix(route.Path, "/{name}") {
		parameter := openapi3.NewPathParameter(nameProperty)
		parameter.Schema = schemaRef("ResourceName")
		parameter.Description = "The name of the resource."
		operationSpec.Parameters = append(operationSpec.Parameters, &openapi3.ParameterRef{Value: parameter})
	}
	for _, name := range route.Query {
		parameter, err := queryParameter(name, description.Fields)
		if err != nil {
			return nil, err
		}
		operationSpec.Parameters = append(operationSpec.Parameters, parameter)
	}
	status, body := http.StatusOK, schemaRef(name)
	result := "The requested resource."
	switch route.Operation {
	case resource.OpCreate:
		status = http.StatusCreated
		operationSpec.Description = "Creates the resource identified by metadata.name. " +
			"Omitted optional fields use their datastore defaults."
		operationSpec.RequestBody = requestBody(name + "Create")
		result = "The created resource."
	case resource.OpGet:
		operationSpec.Description = "Returns the named resource."
		if description.ReadOnly {
			operationSpec.Description += " Hidden catalog entries remain accessible by name. " +
				"An entry without an eligible public view returns 404 even if its backing resource exists."
		}
	case resource.OpUpdate:
		operationSpec.Summary = "Replace " + name
		operationSpec.Description = "Replaces the resource configuration managed by this API. " +
			"Omitted fields owned by this API are removed or reset to defaults. " +
			"The body name must match the path."
		operationSpec.RequestBody = requestBody(name + "Update")
		result = "The resource after applying the desired state."
	case resource.OpList:
		body = schemaRef(name + "List")
		operationSpec.Summary += " resources"
		operationSpec.Description = "Returns one page of resources. " +
			"Read the next page with the returned pageKey while hasMore is true."
		if description.ReadOnly {
			operationSpec.Description += " Hidden entries are excluded. " +
				"Eligible views are filtered and sorted by name before pagination. " +
				"Changes to these results invalidate the page key."
		}
		result = "One page of resources."
	case resource.OpDelete:
		status, body = http.StatusAccepted, nil
		operationSpec.Description = "Requests deletion without waiting for cleanup of the resource and its dependents."
		result = "Deletion was requested."
	}
	if description.ReadOnly {
		operationSpec.Extensions = map[string]any{"x-max-catalog-source-items": resource.MaxCatalogSourceItems}
		operationSpec.Description += fmt.Sprintf(" Any source or dependency collection scanned to build this view "+
			"is limited to %d items before filtering. Exceeding the limit returns 503.", resource.MaxCatalogSourceItems)
	}
	if route.Operation == resource.OpUpdate || route.Operation == resource.OpDelete {
		// A quoted version with at least one character that is not a zero. Request validators cannot
		// decode a parameter whose schema excludes the all-zero versions with "not".
		etag := openapi3.NewStringSchema().WithMinLength(3).
			WithPattern(`^"[^" ,*\t\r\n]*[^" ,*\t\r\n0][^" ,*\t\r\n]*"$`)
		parameter := openapi3.NewHeaderParameter("If-Match").
			WithRequired(true).WithSchema(etag).
			WithDescription("Send exactly one prior ETag including quotes. " +
				"Wildcards, weak ETags, and all-zero versions are rejected.")
		operationSpec.Parameters = append(operationSpec.Parameters, &openapi3.ParameterRef{Value: parameter})
	}
	response := openapi3.NewResponse().WithDescription(result)
	if body != nil {
		response.Content = content(jsonMediaType, body)
	}
	if !description.ReadOnly && route.Operation != resource.OpList && route.Operation != resource.OpDelete {
		header := &openapi3.Header{}
		header.Description = "Quoted resource version. Send it unchanged in If-Match for update or delete."
		header.Schema = &openapi3.SchemaRef{Value: openapi3.NewStringSchema()}
		response.Headers = openapi3.Headers{"ETag": {Value: header}}
	}
	operationSpec.Responses.Set(strconv.Itoa(status), &openapi3.ResponseRef{Value: response})
	return operationSpec, nil
}

func errorStatuses(operationKind resource.Operation) []int {
	common := []int{400, 401, 403, 500, 503}
	switch operationKind {
	case resource.OpCreate:
		return append(common, 408, 409, 413, 415, 422)
	case resource.OpGet:
		return append(common, 404)
	case resource.OpList:
		return append(common, 410)
	case resource.OpUpdate:
		return append(common, 404, 408, 409, 412, 413, 415, 422, 428)
	case resource.OpDelete:
		return append(common, 404, 412, 428)
	default:
		return common
	}
}

func schemaRef(name string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Ref: "#/components/schemas/" + name}
}

func content(mediaType string, schema *openapi3.SchemaRef) openapi3.Content {
	return openapi3.Content{mediaType: &openapi3.MediaType{Schema: schema}}
}

func requestBody(name string) *openapi3.RequestBodyRef {
	body := openapi3.NewRequestBody().WithRequired(true).WithJSONSchemaRef(schemaRef(name))
	body.Description = "The resource identity and complete desired configuration."
	return &openapi3.RequestBodyRef{Value: body}
}

func problemResponse(description string) *openapi3.ResponseRef {
	return &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription(description).
		WithContent(content("application/problem+json", schemaRef("Problem")))}
}

func queryParameter(name string, selectableFields []string) (*openapi3.ParameterRef, error) {
	parameter := openapi3.NewQueryParameter(name)
	switch name {
	case "pageSize":
		parameter.Description = fmt.Sprintf("Number of results (default %d, maximum %d).",
			resource.DefaultPageSize, resource.MaxPageSize)
		parameter.WithSchema(openapi3.NewIntegerSchema().WithMin(1).WithMax(resource.MaxPageSize).
			WithDefault(resource.DefaultPageSize))
	case "pageKey":
		parameter.Description = fmt.Sprintf("Use the pageKey returned by the previous list response. Keep the same filters. "+
			"At most %d UTF-8 bytes.",
			resource.MaxPageKeyBytes)
		parameter.WithSchema(openapi3.NewStringSchema().WithMaxLength(resource.MaxPageKeyBytes))
		parameter.Schema.Value.Extensions = map[string]any{"x-max-utf8-bytes": resource.MaxPageKeyBytes}
		parameter.AllowEmptyValue = true
	case "fieldSelector":
		parameter.Description = fmt.Sprintf("Comma-separated equality filters on: %s. "+
			"Each field may appear once. At most %d UTF-8 bytes.",
			strings.Join(selectableFields, ", "), resource.MaxFieldSelectorBytes)
		parameter.WithSchema(openapi3.NewStringSchema().WithMinLength(1).WithMaxLength(resource.MaxFieldSelectorBytes))
		parameter.Schema.Value.Extensions = map[string]any{"x-max-utf8-bytes": resource.MaxFieldSelectorBytes}
	case "version":
		parameter.Description = "Filter channel versions by major.minor, major.minor.patch, or an exact suffixed version. " +
			"Numeric version components must fit unsigned 64-bit integers."
		parameter.WithSchema(openapi3.NewStringSchema().WithMinLength(1).WithMaxLength(v1.MaxCatalogVersionLength).
			WithPattern(v1.CatalogVersionPattern))
	case "propagationPolicy":
		parameter.Description = "Delete dependent resources after the resource (background) or before it (foreground). " +
			"An omitted or empty value selects background."
		parameter.WithSchema(openapi3.NewStringSchema().WithEnum("background", "foreground", "").WithDefault("background"))
		parameter.AllowEmptyValue = true
	default:
		return nil, fmt.Errorf("undocumented query parameter %q", name)
	}
	return &openapi3.ParameterRef{Value: parameter}, nil
}
