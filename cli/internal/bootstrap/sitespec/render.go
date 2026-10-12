// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// templateYAML is the annotated site spec Render starts from. It is embedded
// rather than read from cli/examples so nvkectl carries it as a shipped binary,
// and it is kept byte-identical in content to that example by
// TestExampleMatchesTemplate.
//
//go:embed template.yaml
var templateYAML []byte

// deniedPaths are the field paths --set must refuse.
//
// registry.apiKey is a credential: the site spec is meant to be committed, so
// the secret is supplied at bootstrap time instead. cluster.nodes comes from
// the node CSV, which carries per-node fields a dotted path cannot express.
func deniedPaths() map[string]string {
	return map[string]string{
		pathAPIKey: "a registry credential does not belong in a file meant to be committed; " +
			"pass it to \"nvkectl bootstrap site\" with --registry-api-key or " + registryAPIKeyEnvName,
		pathNodes: "nodes come from --nodes",
	}
}

// registryAPIKeyEnvName is named here rather than imported from the command
// package so the error can point at it without inverting the dependency.
const registryAPIKeyEnvName = "REGISTRY_API_KEY"

// SettablePaths returns the field paths --set accepts, sorted.
//
// It is derived from the environment bindings rather than listed separately, so
// a field that gains an environment variable becomes settable with it, and an
// exclusion has to be written down in deniedPaths to take effect.
func SettablePaths() []string {
	denied := deniedPaths()
	paths := make([]string, 0, len(envBindings()))
	for _, path := range envBindings() {
		if _, ok := denied[path]; ok {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// ParseNodesCSV reads the node list for "nvkectl generate sitespec".
//
// The first record is a header naming the columns in any order: name and ip are
// required, tpm_ek_thumbprint is optional per row. An unknown column is an
// error rather than being ignored, so a typo does not silently drop a field.
// name identifies the file in error messages.
func ParseNodesCSV(name string, r io.Reader) ([]Node, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: file is empty; expected a header row of %s", name, strings.Join(csvColumns(), ","))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	index, err := csvHeaderIndex(name, header)
	if err != nil {
		return nil, err
	}

	var nodes []Node
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		node := Node{
			Name:            record[index[csvName]],
			IP:              record[index[csvIP]],
			TPMEKThumbprint: valueAt(record, index, csvThumbprint),
		}
		if node.Name == "" {
			return nil, fmt.Errorf("%s line %d: %s is empty", name, reader.InputOffset(), csvName)
		}
		if node.IP == "" {
			return nil, fmt.Errorf("%s line %d: %s is empty", name, reader.InputOffset(), csvIP)
		}
		nodes = append(nodes, node)
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("%s: no node rows after the header", name)
	}
	return nodes, nil
}

// CSV column names. The thumbprint column is spelled in snake_case because a
// CSV header is not YAML; the field it fills is cluster.nodes[].tpmEkThumbprint.
const (
	csvName       = "name"
	csvIP         = "ip"
	csvThumbprint = "tpm_ek_thumbprint"
)

func csvColumns() []string { return []string{csvName, csvIP, csvThumbprint} }

// csvHeaderIndex maps each accepted column to its position, rejecting unknown
// and duplicate columns and a header missing a required one. A UTF-8 byte-order
// mark is stripped: spreadsheets write one and it would otherwise make the
// first column unrecognisable.
func csvHeaderIndex(name string, header []string) (map[string]int, error) {
	index := make(map[string]int, len(header))
	for i, column := range header {
		column = strings.TrimPrefix(strings.TrimSpace(column), "\ufeff")
		switch column {
		case csvName, csvIP, csvThumbprint:
			if _, seen := index[column]; seen {
				return nil, fmt.Errorf("%s: column %q appears more than once in the header", name, column)
			}
			index[column] = i
		default:
			return nil, fmt.Errorf("%s: unknown column %q in the header; accepted columns are %s",
				name, column, strings.Join(csvColumns(), ", "))
		}
	}
	for _, required := range []string{csvName, csvIP} {
		if _, ok := index[required]; !ok {
			return nil, fmt.Errorf("%s: header is missing the %q column", name, required)
		}
	}
	return index, nil
}

func valueAt(record []string, index map[string]int, column string) string {
	i, ok := index[column]
	if !ok || i >= len(record) {
		return ""
	}
	return record[i]
}

// Build assembles a SiteSpec from the inputs "nvkectl generate sitespec" takes
// and validates it, returning the first problem found in the arguments and
// otherwise every field-level problem at once.
//
// It reads no environment variable, so the result depends only on its
// arguments. Load binds the environment because bootstrap has to honour the
// operator's shell; a generator that did would write values nobody typed.
func Build(clusterName string, nodes []Node, sets []string) (*SiteSpec, error) {
	doc, err := templateDoc()
	if err != nil {
		return nil, err
	}
	root := doc.Content[0]
	if err := setScalar(root, pathClusterName, clusterName); err != nil {
		return nil, err
	}
	for _, set := range sets {
		path, value, ok := strings.Cut(set, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: expected path=value", set)
		}
		if err := checkSettable(path); err != nil {
			return nil, err
		}
		if err := setValue(root, path, value); err != nil {
			return nil, err
		}
	}
	if err := setNodes(root, nodes); err != nil {
		return nil, err
	}

	spec, err := decode(root)
	if err != nil {
		return nil, err
	}
	spec.SetDefaults()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

// Render returns spec as an annotated site spec file, preserving the comments
// the template carries.
//
// It decodes its own output and reports an error unless the result matches
// spec, so a value that escaped its field cannot reach the file.
func (spec *SiteSpec) Render() ([]byte, error) {
	doc, err := templateDoc()
	if err != nil {
		return nil, err
	}
	if err := applySpec(doc.Content[0], spec); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return nil, fmt.Errorf("render site spec: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("render site spec: %w", err)
	}

	rendered := buf.Bytes()
	if err := confirmRoundTrip(rendered, spec); err != nil {
		return nil, err
	}
	return rendered, nil
}

// confirmRoundTrip re-reads rendered bytes and compares them to want. It is the
// check that makes "the file says what was validated" a property of Render
// rather than a claim about it.
func confirmRoundTrip(rendered []byte, want *SiteSpec) error {
	got, err := decodeBytes(rendered)
	if err != nil {
		return fmt.Errorf("render produced a file that does not load: %w", err)
	}
	got.SetDefaults()
	if diff := specDiff(got, want); diff != "" {
		return fmt.Errorf("render produced a file that does not match the validated site spec: %s", diff)
	}
	return nil
}
