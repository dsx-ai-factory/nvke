// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Route generation checks reviewed HTTP bindings against descriptors before publishing shared route metadata.

package main

import (
	"bytes"
	"fmt"
	"go/format"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"sigs.k8s.io/yaml"
)

type httpConfig struct {
	Type          string `json:"type"`
	ConfigVersion int    `json:"config_version"`
	HTTP          struct {
		Rules []httpRule `json:"rules"`
	} `json:"http"`
}

type httpRule struct {
	Selector string `json:"selector"`
	Get      string `json:"get"`
	Post     string `json:"post"`
	Put      string `json:"put"`
	Body     string `json:"body"`
}

type httpRoute struct {
	method, path, fullMethod, body string
}

var routePattern = regexp.MustCompile(`^/v1/[a-z][a-z_]*(/\{[a-z_]+\})?$`)

// generateRoutes combines HTTP config with compiled descriptors so route metadata matches the public service.
func generateRoutes(root, descriptorPath string, check bool) error {
	data, err := os.ReadFile(descriptorPath) // #nosec G304 -- Descriptor path supplied by the local build command.
	if err != nil {
		return fmt.Errorf("read protobuf descriptor: %w", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &set); err != nil {
		return fmt.Errorf("decode protobuf descriptor: %w", err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return fmt.Errorf("resolve protobuf descriptor: %w", err)
	}
	config, err := os.ReadFile(filepath.Join(root, contractDir, "gateway.yaml")) // #nosec G304 -- Local build input.
	if err != nil {
		return fmt.Errorf("read gateway configuration: %w", err)
	}
	routes, err := compileRoutes(config, files)
	if err != nil {
		return err
	}
	var source bytes.Buffer
	source.WriteString(generatedHeader)
	source.WriteString(`package clusterv1

// HTTPRoute describes one reviewed protobuf HTTP binding.
type HTTPRoute struct { Method, Path, FullMethod, Body string }

// HTTPRoutes is generated from gateway.yaml and the compiled protobuf service.
var HTTPRoutes = []HTTPRoute{
`)
	for _, route := range routes {
		fmt.Fprintf(&source, "{Method: %q, Path: %q, FullMethod: %q, Body: %q},\n",
			route.method, route.path, route.fullMethod, route.body)
	}
	source.WriteString("}\n")
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		return fmt.Errorf("format HTTP metadata: %w", err)
	}
	return writeOutput(filepath.Join(root, generatedDir, "routes_generated.go"), formatted, check)
}

// compileRoutes verifies complete, unique unary bindings so reviewed HTTP config cannot silently omit methods.
func compileRoutes(data []byte, files *protoregistry.Files) ([]httpRoute, error) {
	var config httpConfig
	if err := yaml.UnmarshalStrict(data, &config); err != nil {
		return nil, fmt.Errorf("decode gateway configuration: %w", err)
	}
	if config.Type != "google.api.Service" || config.ConfigVersion != 3 {
		return nil, fmt.Errorf("expected google.api.Service version 3 gateway configuration")
	}
	seenMethods, seenPaths := map[string]bool{}, map[string]bool{}
	var routes []httpRoute
	for _, rule := range config.HTTP.Rules {
		descriptor, err := files.FindDescriptorByName(protoreflect.FullName(rule.Selector))
		if err != nil {
			return nil, fmt.Errorf("resolve HTTP selector %s: %w", rule.Selector, err)
		}
		method, ok := descriptor.(protoreflect.MethodDescriptor)
		if !ok || method.IsStreamingClient() || method.IsStreamingServer() || seenMethods[rule.Selector] {
			return nil, fmt.Errorf("HTTP selector %s must identify one unbound unary RPC", rule.Selector)
		}
		route, err := compileRoute(rule, method)
		if err != nil {
			return nil, err
		}
		key := route.method + " " + route.path
		if seenPaths[key] {
			return nil, fmt.Errorf("duplicate HTTP route %s", key)
		}
		seenPaths[key], seenMethods[rule.Selector] = true, true
		routes = append(routes, route)
	}
	var missing string
	files.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		if file.Package() != "cluster.v1" {
			return true
		}
		for service := range file.Services().Len() {
			methods := file.Services().Get(service).Methods()
			for i := range methods.Len() {
				if name := string(methods.Get(i).FullName()); !seenMethods[name] {
					missing = name
					return false
				}
			}
		}
		return true
	})
	if missing != "" {
		return nil, fmt.Errorf("public RPC %s has no HTTP binding", missing)
	}
	return routes, nil
}

// compileRoute checks one binding against its request shape so generated routes use supported HTTP semantics.
func compileRoute(rule httpRule, method protoreflect.MethodDescriptor) (httpRoute, error) {
	route := httpRoute{body: rule.Body,
		fullMethod: "/" + string(method.Parent().FullName()) + "/" + string(method.Name())}
	bindings := 0
	paths := map[string]string{http.MethodGet: rule.Get, http.MethodPost: rule.Post, http.MethodPut: rule.Put}
	for verb, path := range paths {
		if path != "" {
			route.method, route.path = verb, path
			bindings++
		}
	}
	if bindings != 1 || !routePattern.MatchString(route.path) ||
		(route.method == http.MethodGet && rule.Body != "") || (route.method != http.MethodGet && rule.Body != "*") {
		return route, fmt.Errorf("unsupported HTTP binding for %s", rule.Selector)
	}
	if start := strings.IndexByte(route.path, '{'); start >= 0 {
		name := route.path[start+1 : len(route.path)-1]
		field := method.Input().Fields().ByName(protoreflect.Name(name))
		if field == nil || field.Kind() != protoreflect.StringKind || field.IsList() {
			return route, fmt.Errorf("HTTP path field %s must identify a request string", name)
		}
	}
	return route, nil
}
