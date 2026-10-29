// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package strictyaml decodes a typed YAML config file, refusing anything a
// lenient decoder would silently drop or reinterpret.
package strictyaml

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	yamlv3 "gopkg.in/yaml.v3"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// Decode decodes raw, which must hold exactly one YAML document, into v. It
// refuses unknown, case-mismatched and duplicate keys. what names the file in
// errors, such as `authentication config "/etc/auth.yaml"`.
func Decode(what string, raw []byte, v any) error {
	if err := requireSingleDocument(what, raw); err != nil {
		return err
	}

	// Decoded with sigs.k8s.io/json, NOT sigs.k8s.io/yaml.UnmarshalStrict, which
	// ends in encoding/json and matches keys case-insensitively. There a key
	// spelled two ways is not a duplicate, and the lowercase spelling wins
	// without an error. In the authN config, pathprefixes beside pathPrefixes
	// admits the whole trust domain.
	jsonBytes, err := yaml.YAMLToJSONStrict(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", what, err)
	}

	strictErrs, err := kjson.UnmarshalStrict(jsonBytes, v)
	if err != nil {
		return fmt.Errorf("parse %s: %w", what, err)
	}
	// Fatal, not advisory: UnmarshalStrict reports a duplicate and still keeps the
	// winning value, so ignoring these would load the file with that value.
	if len(strictErrs) > 0 {
		return fmt.Errorf("parse %s: %w", what, errors.Join(strictErrs...))
	}
	return nil
}

// requireSingleDocument refuses a file that is empty or holds more than one YAML
// document. sigs.k8s.io/yaml has no streaming decoder and silently honors only
// the first, so an appended stanza would have no error and no effect.
func requireSingleDocument(what string, raw []byte) error {
	dec := yamlv3.NewDecoder(bytes.NewReader(raw))

	var first yamlv3.Node
	if err := dec.Decode(&first); err != nil {
		if errors.Is(err, io.EOF) {
			// Named here because a check on the decoded value would report it
			// as a missing apiVersion and send the operator looking elsewhere.
			return fmt.Errorf("%s is empty", what)
		}
		return fmt.Errorf("parse %s: %w", what, err)
	}

	var extra yamlv3.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("parse %s: %w", what, err)
		}
		return fmt.Errorf("%s contains more than one YAML document", what)
	}
	return nil
}
