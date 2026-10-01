// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwedeploy

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The controller's name is written in cweNamespace and in three embedded
// manifests. A copy left at the legacy name installs cleanly and then fails as
// a Service with no endpoints or a controller with no SPIFFE identity, so this
// checks every place the bootstrap names it.
const legacyControllerName = "cloud-workflow-engine"

func TestBootstrapNamesNoLegacyController(t *testing.T) {
	for _, file := range []string{spireValuesFile, cweValuesFile, cweNodePortFile} {
		body, err := fs.ReadFile(manifestsFS, file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, legacyControllerName) {
				t.Errorf("%s names the legacy controller: %q", file, line)
			}
		}
	}

	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	for _, call := range runner.calls {
		if args := joinArgs(call.args); strings.Contains(args, legacyControllerName) {
			t.Errorf("%s call names the legacy controller: %s", call.name, args)
		}
	}
	for _, manifest := range runner.applied {
		if strings.Contains(manifest, legacyControllerName) {
			t.Errorf("applied manifest names the legacy controller:\n%s", manifest)
		}
	}
}

// TestControllerValuesNameEveryObject pins the chart values that have to carry
// the name besides the fullname: nameOverride is the pod label the external
// Service selects on, and the chart pins serviceAccount.name instead of
// deriving it, which the SPIRE admin ID and the controller's SPIFFE ID name.
func TestControllerValuesNameEveryObject(t *testing.T) {
	body, err := fs.ReadFile(manifestsFS, cweValuesFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"fullnameOverride: " + cweNamespace + "\n",
		"nameOverride: " + cweNamespace + "\n",
		"namespaceOverride: " + cweNamespace + "\n",
		"serviceAccount:\n  name: " + cweNamespace + "\n",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s does not set %q", cweValuesFile, strings.TrimSpace(want))
		}
	}
}

// TestNodePortAndSPIRENameTheController pins the two other manifests that carry
// the name. The external Service has to select the controller's pods, and the
// SPIRE namespace selectors have to keep the controller's namespace out of the
// default identity and issue it the nke-service one; a typo in any of them
// installs cleanly and fails later.
func TestNodePortAndSPIRENameTheController(t *testing.T) {
	path, cleanup, err := writeEmbeddedTemplate(cweNodePortFile, struct{ Port int32 }{DefaultCWEPort})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var service struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Selector map[string]string `yaml:"selector"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(body, &service); err != nil {
		t.Fatalf("%s: %v", cweNodePortFile, err)
	}
	if want := cweNamespace + "-external"; service.Metadata.Name != want {
		t.Errorf("%s metadata.name = %q, want %q", cweNodePortFile, service.Metadata.Name, want)
	}
	for _, key := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance"} {
		if got := service.Spec.Selector[key]; got != cweNamespace {
			t.Errorf("%s selector %s = %q, want %q", cweNodePortFile, key, got, cweNamespace)
		}
	}

	body, err = fs.ReadFile(manifestsFS, spireValuesFile)
	if err != nil {
		t.Fatal(err)
	}
	type selector struct {
		NamespaceSelector struct {
			MatchExpressions []struct {
				Key      string   `yaml:"key"`
				Operator string   `yaml:"operator"`
				Values   []string `yaml:"values"`
			} `yaml:"matchExpressions"`
		} `yaml:"namespaceSelector"`
	}
	var values struct {
		SPIREServer struct {
			ControllerManager struct {
				Identities struct {
					ClusterSPIFFEIDs map[string]selector `yaml:"clusterSPIFFEIDs"`
				} `yaml:"identities"`
			} `yaml:"controllerManager"`
		} `yaml:"spire-server"`
	}
	if err := yaml.Unmarshal(body, &values); err != nil {
		t.Fatalf("%s: %v", spireValuesFile, err)
	}
	ids := values.SPIREServer.ControllerManager.Identities.ClusterSPIFFEIDs
	for id, operator := range map[string]string{"default": "NotIn", "nke-service": "In"} {
		exprs := ids[id].NamespaceSelector.MatchExpressions
		if len(exprs) != 1 || exprs[0].Key != "kubernetes.io/metadata.name" || exprs[0].Operator != operator ||
			len(exprs[0].Values) != 1 || exprs[0].Values[0] != cweNamespace {
			t.Errorf("%s clusterSPIFFEIDs.%s.namespaceSelector = %+v, want kubernetes.io/metadata.name %s [%s]",
				spireValuesFile, id, exprs, operator, cweNamespace)
		}
	}
}
