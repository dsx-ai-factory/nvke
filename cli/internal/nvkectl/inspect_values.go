// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// newInspectValuesCmd prints the top-level keys of a Helm values file, so the
// install scripts can ask whether a file sets a key without parsing YAML
// themselves.
func newInspectValuesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "values FILE",
		Short: "Print the top-level keys of a Helm values file, one per line",
		Long: `Read a Helm values file as YAML, which includes JSON, and print the keys of
its top-level mapping, one per line, in file order. A key that a YAML merge key
(<<) brings in is listed too. A file with no content has no keys. A file whose
top level is not a mapping, or that holds more than one document, is an error
naming the file; nothing is printed then.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			keys, err := valuesKeys(data)
			if err != nil {
				return fmt.Errorf("values file %s: %w", args[0], err)
			}
			for _, key := range keys {
				fmt.Fprintln(cmd.OutOrStdout(), key)
			}
			return nil
		},
	}
}

// valuesKeys returns the keys of the top-level mapping of a values file. A
// repeated key is listed each time, since Helm reads the last one and a check
// asking whether a key is set must not miss any of them.
func valuesKeys(data []byte) ([]string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root *yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
			continue
		}
		if root != nil {
			return nil, errors.New("more than one document")
		}
		root = doc.Content[0]
	}
	if root == nil {
		return nil, nil
	}
	return mappingKeys(root)
}

// mappingKeys lists the keys of a mapping node, following an alias and the
// mappings a merge key brings in. A null top level, which is what a file with
// only comments or a bare "~" holds, has no keys.
func mappingKeys(node *yaml.Node) ([]string, error) {
	for node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("the top level is not a mapping")
	}
	var keys []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag != "!!merge" {
			keys = append(keys, key.Value)
			continue
		}
		sources := []*yaml.Node{value}
		if value.Kind == yaml.SequenceNode {
			sources = value.Content
		}
		for _, source := range sources {
			merged, err := mappingKeys(source)
			if err != nil {
				return nil, fmt.Errorf("a merge key: %w", err)
			}
			keys = append(keys, merged...)
		}
	}
	return keys, nil
}
