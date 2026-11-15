// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"reflect"
	"slices"
)

// Description exposes a registration's contract without contacting the datastore.
type Description struct {
	Info       ResourceInfo
	PublicType reflect.Type
	ListType   reflect.Type
	SourceType reflect.Type
	Routes     []Route
	ReadOnly   bool
	Fields     []string
}

// Route describes one enabled HTTP operation, using the server's route table.
type Route struct {
	Operation Operation
	Method    string
	Path      string
	// Query lists the supported query parameters for this operation.
	Query []string
}

// Describe returns the resource types and routes selected by this registration.
func (r Registration) Describe() Description {
	description := r.description
	description.Info.Verbs = slices.Clone(description.Info.Verbs)
	description.Routes = slices.Clone(description.Routes)
	for i := range description.Routes {
		description.Routes[i].Query = slices.Clone(description.Routes[i].Query)
	}
	description.Fields = slices.Clone(description.Fields)
	return description
}

func describe[E, I any](path string, info ResourceInfo, readOnly bool, fields []string) Description {
	var source I
	_, versionFilter := any(source).(versionFilteredSource[I])
	description := Description{
		Info: info, PublicType: reflect.TypeFor[E](), SourceType: reflect.TypeFor[I](),
		ListType: reflect.TypeFor[List[E]](),
		ReadOnly: readOnly, Fields: fields,
	}
	for _, route := range resourceRoutes {
		if slices.Contains(info.Verbs, route.op) {
			description.Routes = append(description.Routes, Route{
				Operation: route.op, Method: route.method, Path: path + route.suffix,
				Query: queryParameters(route.op, len(fields) != 0, readOnly && versionFilter),
			})
		}
	}
	return description
}
