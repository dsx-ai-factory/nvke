//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func base() Vars {
	return Vars{
		Hostname: "n1", PasswordHash: "$6$x$y",
	}
}

// Both provision modes must render to parseable YAML with the runcmd items the
// mode promises. Indentation is the thing string substitution breaks.
func TestProvisionModesRenderValidYAML(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		want        []string
	}{
		{
			"apt", AptProvision("https://r/", "https://g/", "nvke-noble", "main", "pkg", "6"),
			[]string{"apt-get install -y -o DPkg::Lock::Timeout=600 pkg=6 nke-agent"},
		},
		{
			"community", CommunityProvision("http://172.30.0.1:8080/install-k8s.sh", "http://172.30.0.1:8080/a.deb"),
			[]string{"install-k8s.sh", "nke-agent.deb"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := base()
			v.K8sProvision = tc.block
			out, err := Render("../../testdata/user-data.yml.tmpl", v)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			var doc struct {
				Runcmd []string `yaml:"runcmd"`
			}
			if err := yaml.Unmarshal([]byte(strings.TrimPrefix(out, "#cloud-config\n")), &doc); err != nil {
				t.Fatalf("rendered output is not valid YAML: %v", err)
			}
			if len(doc.Runcmd) == 0 {
				t.Fatal("runcmd is empty")
			}
			joined := strings.Join(doc.Runcmd, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("runcmd missing %q; got:\n%s", w, joined)
				}
			}
			if strings.Contains(out, "__") {
				t.Errorf("unresolved placeholder remains")
			}
			_ = os.Stdout
		})
	}
}
