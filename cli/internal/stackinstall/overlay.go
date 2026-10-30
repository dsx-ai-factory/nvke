// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"errors"
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
	var committed struct {
		Provider struct {
			Config struct {
				AdditionalDomains []string `yaml:"additionalDomains"`
			} `yaml:"config"`
			TLS struct {
				CertManager struct {
					Certificate struct {
						DNSNames []string `yaml:"dnsNames"`
					} `yaml:"certificate"`
				} `yaml:"certManager"`
			} `yaml:"tls"`
		} `yaml:"spiffe-oidc-discovery-provider"`
	}
	if err := readValues("values/spire.yaml", &committed); err != nil {
		return nil, err
	}
	domains, names := committed.Provider.Config.AdditionalDomains, committed.Provider.TLS.CertManager.Certificate.DNSNames
	if len(domains) == 0 || len(names) == 0 {
		return nil, errors.New("values/spire.yaml: the OIDC provider needs additionalDomains and dnsNames")
	}
	certificate := map[string]any{}
	if _, err := netip.ParseAddr(v.address); err == nil {
		certificate["ipAddresses"] = []string{v.address}
	} else {
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

// adminID is the controller's SPIFFE ID, from values/controller.yaml.
func adminID(trustDomain string) (string, error) {
	var c struct {
		ServiceAccount struct {
			Name string `yaml:"name"`
		} `yaml:"serviceAccount"`
		Spiffe struct {
			ServicePathSegment string `yaml:"servicePathSegment"`
		} `yaml:"spiffe"`
	}
	if err := readValues("values/controller.yaml", &c); err != nil {
		return "", err
	}
	if c.Spiffe.ServicePathSegment == "" || c.ServiceAccount.Name == "" {
		return "", errors.New("values/controller.yaml: spiffe.servicePathSegment and serviceAccount.name are required")
	}
	return "spiffe://" + trustDomain + "/" + c.Spiffe.ServicePathSegment + "/" + c.ServiceAccount.Name, nil
}

func appendMissing(list []string, value string) []string {
	if slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}

// readValues decodes an embedded values file into v.
func readValues(file string, v any) error {
	data, err := fs.ReadFile(stackFS, file)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}
