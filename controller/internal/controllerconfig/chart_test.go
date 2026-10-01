// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
)

// renderedChartDirEnv names the directory of `helm template` outputs that
// TestRenderedChart checks, one file per value set.
// hack/chart-controller-config-assertions.sh renders them and sets it.
const renderedChartDirEnv = "NKE_RENDERED_CHART_DIR"

// TestRenderedChart checks the chart's nke-controller-config against the
// controller, once per rendered value set: the document loads, defines every
// key, and resolves each migrated key to the value the controller derives from
// the same render's environment variables today. Ordinary test runs skip it.
func TestRenderedChart(t *testing.T) {
	t.Parallel()

	dir := os.Getenv(renderedChartDirEnv)
	if dir == "" {
		t.Skip(renderedChartDirEnv + " is unset; hack/chart-controller-config-assertions.sh sets it")
	}

	renders, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, renders, "no renders in %s", dir)

	for _, render := range renders {
		t.Run(strings.TrimSuffix(filepath.Base(render), ".yaml"), func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(render)
			require.NoError(t, err)
			document, environment := splitRender(t, raw)

			fromFile, err := controllerconfig.Load(writeConfig(t, document))
			require.NoError(t, err, "the rendered config.yaml does not load")

			present := documentKeys(t, document)
			for _, key := range schemaKeys(reflect.TypeFor[controllerconfig.Config](), "") {
				if !present[key] {
					t.Errorf("the rendered config.yaml leaves out %s, which would decode as its zero value", key)
				}
			}

			// What the controller runs on today: the same environment parsed and
			// validated as at startup, which trims and normalizes in place.
			fromEnv, err := env.ParseAsWithOptions[config.Config](env.Options{
				Environment:         environment,
				DefaultValueTagName: "default",
			})
			require.NoError(t, err)
			require.NoError(t, fromEnv.Validate())

			for name, key := range migrated {
				want := setting(fieldByEnv(reflect.ValueOf(fromEnv), "", name))
				got := setting(fieldByKey(reflect.ValueOf(*fromFile), key))
				if !reflect.DeepEqual(want, got) {
					t.Errorf("%s is %s, but the controller derives %s from %s", key, show(got), show(want), name)
				}
			}
		})
	}
}

// splitRender returns the nke-controller-config document and the controller
// container's literal environment variables from one `helm template` output.
// Variables set through valueFrom are left out: none of them migrate.
func splitRender(t *testing.T, raw []byte) (string, map[string]string) {
	t.Helper()

	var document string
	var environment map[string]string
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		manifest, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)

		var meta metav1.TypeMeta
		require.NoError(t, yaml.Unmarshal(manifest, &meta))
		switch meta.Kind {
		case "ConfigMap":
			var configMap corev1.ConfigMap
			require.NoError(t, yaml.Unmarshal(manifest, &configMap))
			if configMap.Name == "nke-controller-config" {
				require.Empty(t, document, "the render has more than one nke-controller-config")
				document = configMap.Data["config.yaml"]
			}
		case "Deployment":
			require.Nil(t, environment, "the render has more than one Deployment")
			var deployment appsv1.Deployment
			require.NoError(t, yaml.Unmarshal(manifest, &deployment))
			containers := deployment.Spec.Template.Spec.Containers
			require.Len(t, containers, 1, "expected one controller container in %s", deployment.Name)
			environment = make(map[string]string, len(containers[0].Env))
			for _, variable := range containers[0].Env {
				if variable.ValueFrom == nil {
					environment[variable.Name] = variable.Value
				}
			}
		}
	}
	require.NotEmpty(t, document, "the render has no nke-controller-config")
	require.NotNil(t, environment, "the render has no controller Deployment")
	return document, environment
}

// documentKeys returns every dotted key path the YAML document sets, at every
// depth, so a nested key counts as present along with each of its parents.
func documentKeys(t *testing.T, document string) map[string]bool {
	t.Helper()

	var tree map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(document), &tree))
	keys := make(map[string]bool)
	var walk func(node map[string]any, prefix string)
	walk = func(node map[string]any, prefix string) {
		for name, child := range node {
			keys[prefix+name] = true
			if nested, ok := child.(map[string]any); ok {
				walk(nested, prefix+name+".")
			}
		}
	}
	walk(tree, "")
	return keys
}

// fieldByEnv finds the config.Config field that caarlos0/env fills from name.
func fieldByEnv(v reflect.Value, prefix, name string) reflect.Value {
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if tag, ok := field.Tag.Lookup("env"); ok {
			if envName, _, _ := strings.Cut(tag, ","); prefix+envName == name {
				return v.Field(i)
			}
			continue
		}
		if field.Type.Kind() == reflect.Struct {
			if found := fieldByEnv(v.Field(i), prefix+field.Tag.Get("envPrefix"), name); found.IsValid() {
				return found
			}
		}
	}
	return reflect.Value{}
}

// fieldByKey finds the controllerconfig.Config field at a dotted config.yaml key.
func fieldByKey(v reflect.Value, key string) reflect.Value {
	head, rest, nested := strings.Cut(key, ".")
	for i := range v.NumField() {
		if name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ","); name == head {
			if nested {
				return fieldByKey(v.Field(i), rest)
			}
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// setting reduces a field from either struct to a comparable form: durations
// to time.Duration, string kinds to string, and an empty slice to the same
// value whether it decoded as nil or as [].
func setting(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	switch value := v.Interface().(type) {
	case metav1.Duration:
		return value.Duration
	case time.Duration:
		return value
	case []string:
		return append([]string{}, value...)
	}
	// A named string type such as config.SpireAttestationMethod compares as
	// its string. Bools and ints already share a type on both sides.
	if v.Kind() == reflect.String {
		return v.String()
	}
	return v.Interface()
}

// show formats a setting for a failure message: a duration as its Go string,
// anything else with %#v, so an empty string is visible as "".
func show(value any) string {
	if duration, ok := value.(time.Duration); ok {
		return duration.String()
	}
	return fmt.Sprintf("%#v", value)
}
