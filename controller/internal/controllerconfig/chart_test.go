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
	"path"
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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

// renderedChartDirEnv names the directory of `helm template` outputs that
// TestRenderedChart checks, one file per value set.
// hack/chart-controller-config-assertions.sh renders them and sets it.
const renderedChartDirEnv = "NKE_RENDERED_CHART_DIR"

// TestRenderedChart checks the chart's nvke-controller-config against the
// controller, once per rendered value set: the document loads, defines every
// key, is mounted where CONTROLLER_CONFIG_PATH points, and together with the
// render's environment gives the controller a configuration that validates.
// Ordinary test runs skip it.
func TestRenderedChart(t *testing.T) {
	t.Parallel()

	dir := os.Getenv(renderedChartDirEnv)
	if dir == "" {
		t.Skip(renderedChartDirEnv + " is unset; hack/chart-controller-config-assertions.sh sets it")
	}

	renders, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, renders, "no renders in %s", dir)
	require.Contains(t, renders, filepath.Join(dir, "defaults.yaml"), "the script no longer renders the chart's defaults")

	for _, render := range renders {
		t.Run(strings.TrimSuffix(filepath.Base(render), ".yaml"), func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(render)
			require.NoError(t, err)
			document, environment, deployment := splitRender(t, raw)
			checkMounted(t, deployment, environment)

			fromFile, err := controllerconfig.Load(writeConfig(t, document))
			require.NoError(t, err, "the rendered config.yaml does not load")

			present := documentKeys(t, document)
			for _, key := range schemaKeys(reflect.TypeFor[controllerconfig.Config](), "") {
				if !present[key] {
					t.Errorf("the rendered config.yaml leaves out %s, which would decode as its zero value", key)
				}
			}

			// The controller's startup sequence: the environment, then the file.
			cfg, err := env.ParseAsWithOptions[config.Config](env.Options{
				Environment:         environment,
				DefaultValueTagName: "default",
			})
			require.NoError(t, err)
			fromFile.ApplyTo(&cfg)
			// Explicit zero must survive rendering. The runtime policy only
			// checks the bounds of an enabled feature, so a zero threshold
			// under disabled renewal loads. Values the controller refuses are
			// refused at render, so every render here must validate.
			if strings.TrimSuffix(filepath.Base(render), ".yaml") == "explicit-false-and-zero" {
				require.Zero(t, cfg.CertRenewalThresholdDays)
			}
			require.NoError(t, cfg.Validate())

			// Both sources at once is a startup failure, so no render may produce it.
			require.NoError(t, fromFile.CheckAuthSource(environment["AUTH_CONFIG_PATH"]))

			if filepath.Base(render) == "api-auth-inline.yaml" {
				require.True(t, fromFile.APIAuth.Enabled, "the controller reads the chart's apiAuth.config from config.yaml")
				require.NotContains(t, environment, "AUTH_CONFIG_PATH")
			}
			if filepath.Base(render) == "api-auth-permissive.yaml" {
				require.False(t, fromFile.APIAuth.Enabled, "a ConfigMap managed elsewhere stays on AUTH_CONFIG_PATH")
				require.Contains(t, environment, "AUTH_CONFIG_PATH")
			}

			if filepath.Base(render) == "machine-metadata-allowlists.yaml" {
				require.Equal(t, []string{"cluster.x-k8s.io/custom"}, cfg.MachineMetadataAllowlistedLabelKeys)
				require.Equal(t, []string{"nke.nvidia.com/profile"}, cfg.MachineMetadataAllowlistedAnnotationKeys)
				require.Empty(t, cfg.NodeMetadataAllowlistedLabelKeys)
			}

			if filepath.Base(render) == "readiness-custom.yaml" {
				require.Equal(t, config.AgentReadinessConfig{
					Workers: 4, Timeout: 10 * time.Second, Interval: 90 * time.Second,
				}, cfg.AgentReadiness)
			}

			if filepath.Base(render) == "dev-without-vip.yaml" {
				require.True(t, cfg.AllowControlPlaneWithoutVIP)
			}
			if filepath.Base(render) == "defaults.yaml" {
				require.False(t, cfg.AllowControlPlaneWithoutVIP)
				checkLocalAgreesWithChart(t, fromFile)
			}
		})
	}
}

// splitRender returns the nvke-controller-config document, the controller
// container's literal environment variables, and the controller Deployment from
// one `helm template` output. Variables set through valueFrom are left out:
// none of them migrate.
func splitRender(t *testing.T, raw []byte) (string, map[string]string, *appsv1.Deployment) {
	t.Helper()

	var document string
	var environment map[string]string
	var controller *appsv1.Deployment
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
			if configMap.Name == "nvke-controller-config" {
				require.Empty(t, document, "the render has more than one nvke-controller-config")
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
			controller = &deployment
		}
	}
	require.NotEmpty(t, document, "the render has no nvke-controller-config")
	require.NotNil(t, environment, "the render has no controller Deployment")
	return document, environment, controller
}

// checkMounted requires the Deployment to mount nvke-controller-config, read-only
// and not optional, at the path CONTROLLER_CONFIG_PATH names, and Reloader to
// restart the controller when it changes.
func checkMounted(t *testing.T, deployment *appsv1.Deployment, environment map[string]string) {
	t.Helper()

	var volume string
	for _, v := range deployment.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == "nvke-controller-config" {
			require.Empty(t, volume, "more than one volume mounts nvke-controller-config")
			require.False(t, ptr.Deref(v.ConfigMap.Optional, false), "the nvke-controller-config volume is optional")
			volume = v.Name
		}
	}
	require.NotEmpty(t, volume, "no volume mounts nvke-controller-config")

	var mountPath string
	for _, m := range deployment.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == volume {
			require.Empty(t, mountPath, "the controller mounts nvke-controller-config more than once")
			require.True(t, m.ReadOnly, "nvke-controller-config is mounted writable")
			mountPath = m.MountPath
		}
	}
	require.NotEmpty(t, mountPath, "the controller does not mount nvke-controller-config")
	require.Equal(t, path.Join(mountPath, "config.yaml"), environment["CONTROLLER_CONFIG_PATH"])

	watched := strings.Split(deployment.Annotations["configmap.reloader.stakater.com/reload"], ",")
	require.Contains(t, watched, "nvke-controller-config", "Reloader does not watch nvke-controller-config")
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

// localControllerConfig is the file host-local runs and the gofail e2e worker
// start with.
const localControllerConfig = "../../hack/local-controller-config.yaml"

func TestLocalControllerConfig(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(localControllerConfig)
	require.NoError(t, err)
	local, err := controllerconfig.Load(localControllerConfig)
	require.NoError(t, err, "%s does not load", localControllerConfig)

	// The gofail e2e worker edits the loaded file and writes it back out.
	encoded, err := yaml.Marshal(local)
	require.NoError(t, err)
	reloaded, err := controllerconfig.Load(writeConfig(t, string(encoded)))
	require.NoError(t, err, "a marshaled controllerconfig.Config does not load")
	require.Equal(t, local, reloaded)

	present := documentKeys(t, string(raw))
	for _, key := range schemaKeys(reflect.TypeFor[controllerconfig.Config](), "") {
		if !present[key] {
			t.Errorf("%s leaves out %s, which would decode as its zero value", localControllerConfig, key)
		}
	}
}

// Not parallel: the configuration comes from the process environment.
func TestLoadStartupConfig(t *testing.T) {
	t.Setenv("CNPG_SUPERUSER_PASSWORD", "not-the-dev-default")
	t.Setenv("LOG_LEVEL", "debug")

	t.Run("environment and file together", func(t *testing.T) {
		t.Setenv("CONTROLLER_CONFIG_PATH", localControllerConfig)

		cfg, file, err := controllerconfig.LoadStartupConfig()
		require.NoError(t, err)
		require.NotNil(t, file)
		require.Equal(t, "debug", cfg.LogLevel, "from the environment")
		require.Equal(t, "localhost:7233", cfg.TemporalHostPort, "from the file")
		require.Equal(t, file.ProfilesDir, cfg.ProfilesDir)
	})

	t.Run("a missing file is an error", func(t *testing.T) {
		t.Setenv("CONTROLLER_CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))

		_, _, err := controllerconfig.LoadStartupConfig()
		require.ErrorContains(t, err, "absent.yaml")
	})

	t.Run("apiAuth and AUTH_CONFIG_PATH together are refused", func(t *testing.T) {
		local, err := controllerconfig.Load(localControllerConfig)
		require.NoError(t, err)
		local.APIAuth = exampleAPIAuth()
		encoded, err := yaml.Marshal(local)
		require.NoError(t, err)
		t.Setenv("CONTROLLER_CONFIG_PATH", writeConfig(t, string(encoded)))
		t.Setenv("AUTH_CONFIG_PATH", "/etc/nke/auth/auth.yaml")

		_, _, err = controllerconfig.LoadStartupConfig()
		require.ErrorContains(t, err, "configured twice")
	})
}

// localOnly lists the keys where hack/local-controller-config.yaml deliberately
// differs from what the chart renders by default, with the reason. Every other
// key must agree, so a default changed in one place cannot quietly diverge
// between a host-local run and a deployment.
var localOnly = map[string]string{
	"metricsListenAddress":       "a host-local controller keeps metrics on the loopback",
	"nicoControllersEnabled":     "host-local runs do not install CAPNICo, so the NICo controllers stay off",
	"profilesDir":                "host-local runs use the fixture profiles",
	"runbooksDir":                "host-local runs use the fixture runbooks",
	"spire.serverPort":           "the local SPIRE server port-forward, not the site's NodePort",
	"spire.spiffeEndpointSocket": "the socket the local SPIRE agent opens, not the CSI driver's",
	"temporal.hostPort":          "the local Temporal dev server",
	"tenantAccessEnabled":        "off for host-local runs, as it was before the file existed",
}

// checkLocalAgreesWithChart compares the chart's default render with
// hack/local-controller-config.yaml key by key.
func checkLocalAgreesWithChart(t *testing.T, chart *controllerconfig.Config) {
	t.Helper()

	local, err := controllerconfig.Load(localControllerConfig)
	require.NoError(t, err)
	for _, key := range schemaKeys(reflect.TypeFor[controllerconfig.Config](), "") {
		chartValue := fileField(t, reflect.ValueOf(chart).Elem(), key).Interface()
		localValue := fileField(t, reflect.ValueOf(local).Elem(), key).Interface()
		_, allowed := localOnly[key]
		switch differs := !reflect.DeepEqual(chartValue, localValue); {
		case differs && !allowed:
			t.Errorf("%s is %s in %s but %s in the chart's default render; align them, or add it to localOnly with a reason",
				key, show(localValue), localControllerConfig, show(chartValue))
		case !differs && allowed:
			t.Errorf("localOnly lists %s, but %s and the chart's default render agree on it; remove the entry", key, localControllerConfig)
		}
	}
}

// show formats a setting for a failure message: a duration as its Go string,
// anything else with %#v, so an empty string is visible as "".
func show(value any) string {
	if duration, ok := value.(metav1.Duration); ok {
		return duration.Duration.String()
	}
	return fmt.Sprintf("%#v", value)
}
