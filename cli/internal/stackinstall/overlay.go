// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"fmt"
	"io/fs"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	imageDigest     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	imageRepository = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*$`)
)

// oidcNodePort is the NodePort of manifests/spire-oidc-provider-nodeport.yaml.
const oidcNodePort = 30443

// temporalImages are the temporal chart's default repositories. The chart has
// no registry key, so a mirror goes into the repository.
var temporalImages = map[string]string{
	"server":     "temporalio/server",
	"admintools": "temporalio/admin-tools",
	"web":        "temporalio/ui",
}

// overlay returns the input-derived values for the chart that takes
// values/<values>.yaml, or nil when it takes none.
func overlay(values string, v inputs) (map[string]any, error) {
	switch values {
	case "controller":
		return controllerOverlay(v), nil
	case "spire":
		return spireOverlay(v)
	case "postgresql", "nico-prereqs":
		return postgresqlOverlay(v.mirror), nil
	case "temporal":
		return temporalOverlay(v.mirror), nil
	default:
		return nil, nil
	}
}

func controllerOverlay(v inputs) map[string]any {
	return map[string]any{
		"image":        imageValues(v.cwe),
		"spiffe":       map[string]any{"trustDomain": v.trustDomain, "spire": map[string]any{"serverAddress": v.address}},
		"nvkeOIDCAuth": map[string]any{"discoveryURL": fmt.Sprintf("https://%s:%d/.well-known/openid-configuration", v.address, oidcNodePort)},
	}
}

func spireOverlay(v inputs) (map[string]any, error) {
	admin, err := adminID(v.trustDomain)
	if err != nil {
		return nil, err
	}
	global := map[string]any{
		"trustDomain": v.trustDomain,
		"clusterName": v.clusterName,
		// The chart's CA subject defaults to example.org.
		"caSubject": map[string]any{"commonName": v.trustDomain},
	}
	if v.mirror != "" {
		global["image"] = map[string]any{"registry": v.mirror}
	}
	server := map[string]any{
		"adminIDs":     []string{admin},
		"nodeAttestor": map[string]any{"tpmDirect": map[string]any{"checksum": v.checksum}},
	}
	if v.upstreamCA {
		// data.bundle only makes the chart set bundle_file_path; with create
		// false it writes no Secret.
		server["upstreamAuthority"] = map[string]any{"disk": map[string]any{
			"enabled": true,
			"secret":  map[string]any{"create": false, "name": "spire-upstream-ca", "data": map[string]any{"bundle": "bundle.crt"}},
		}}
	}
	// A values file replaces a list, so the committed entries are restated.
	domains, err := committedList("values/spire.yaml", "spiffe-oidc-discovery-provider", "config", "additionalDomains")
	if err != nil {
		return nil, err
	}
	certificate := map[string]any{}
	if _, err := netip.ParseAddr(v.address); err == nil {
		certificate["ipAddresses"] = []string{v.address}
	} else {
		names, err := committedList("values/spire.yaml", "spiffe-oidc-discovery-provider", "tls", "certManager", "certificate", "dnsNames")
		if err != nil {
			return nil, err
		}
		certificate["dnsNames"] = appendMissing(names, v.address)
	}
	return map[string]any{
		"global":       map[string]any{"spire": global},
		"spire-server": server,
		"spiffe-oidc-discovery-provider": map[string]any{
			"config": map[string]any{
				"jwksUri":           fmt.Sprintf("https://%s:%d/keys", v.address, oidcNodePort),
				"additionalDomains": appendMissing(domains, v.address),
			},
			"tls": map[string]any{"certManager": map[string]any{"certificate": certificate}},
		},
	}, nil
}

// postgresqlOverlay serves the Bitnami chart from the mirror. The chart
// refuses an image outside its own registry unless told so.
func postgresqlOverlay(mirror string) map[string]any {
	if mirror == "" {
		return nil
	}
	return map[string]any{"global": map[string]any{
		"imageRegistry": mirror,
		"security":      map[string]any{"allowInsecureImages": true},
	}}
}

func temporalOverlay(mirror string) map[string]any {
	if mirror == "" {
		return nil
	}
	values := map[string]any{}
	for key, repo := range temporalImages {
		values[key] = map[string]any{"image": map[string]any{"repository": mirror + "/" + repo}}
	}
	return values
}

// imageValues splits <repository>@sha256:<digest> into the chart's image
// values, or returns nil for any other shape. The tag is cleared; the chart
// falls back to appVersion and the digest governs the pull.
func imageValues(ref string) map[string]any {
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok || !imageDigest.MatchString(digest) || !imageRepository.MatchString(repo) {
		return nil
	}
	if colon := strings.LastIndex(repo, ":"); colon > strings.LastIndex(repo, "/") {
		repo = repo[:colon]
	}
	return map[string]any{"repository": repo, "digest": digest, "tag": ""}
}

// adminID is the controller's SPIFFE ID: the path segment and the service
// account come from values/controller.yaml.
func adminID(trustDomain string) (string, error) {
	segment, err := committedScalar("values/controller.yaml", "spiffe", "servicePathSegment")
	if err != nil {
		return "", err
	}
	account, err := committedScalar("values/controller.yaml", "serviceAccount", "name")
	if err != nil {
		return "", err
	}
	return "spiffe://" + trustDomain + "/" + segment + "/" + account, nil
}

func appendMissing(list []string, value string) []string {
	if slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}

func committedScalar(file string, keys ...string) (string, error) {
	n, err := committedNode(file, keys...)
	if err != nil {
		return "", err
	}
	if n.Kind != yaml.ScalarNode || n.Value == "" {
		return "", fmt.Errorf("%s: %s is not a non-empty string", file, strings.Join(keys, "."))
	}
	return n.Value, nil
}

func committedList(file string, keys ...string) ([]string, error) {
	n, err := committedNode(file, keys...)
	if err != nil {
		return nil, err
	}
	var list []string
	if err := n.Decode(&list); err != nil || len(list) == 0 {
		return nil, fmt.Errorf("%s: %s is not a non-empty list of strings", file, strings.Join(keys, "."))
	}
	return list, nil
}

// committedNode reads a value from an embedded values file as a node, so a
// scalar keeps its text.
func committedNode(file string, keys ...string) (*yaml.Node, error) {
	data, err := fs.ReadFile(stackFS, file)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	node := &doc
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	for _, key := range keys {
		if node == nil || node.Kind != yaml.MappingNode {
			node = nil
			break
		}
		var found *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				found = node.Content[i+1]
				break
			}
		}
		node = found
	}
	if node == nil {
		return nil, fmt.Errorf("%s has no %s", file, strings.Join(keys, "."))
	}
	return node, nil
}
