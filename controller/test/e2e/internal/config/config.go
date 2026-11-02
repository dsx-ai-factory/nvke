// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package config holds the minimal, file-based, defaults-first configuration
// for the OIDC-SPIRE E2E suite. A missing config file is not an error: every
// field has a default so the suite runs zero-config against a local cluster.
package config

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Config is the E2E suite configuration.
type Config struct {
	Kube struct {
		Kubeconfig string `json:"kubeconfig"` // "" -> default loading (KUBECONFIG/~/.kube/config)
		Context    string `json:"context"`    // "" -> current-context
	} `json:"kube"`
	Cluster struct {
		CAPDNodeImage string `json:"capdNodeImage"` // default "cwe-capd-kindest-node:local"
	} `json:"cluster"`
	Lifecycle struct {
		// SkipDelete keeps what a run created, pass or fail. On a lane that
		// builds its own site it holds the site node too, since the tenants it
		// keeps are Forge instances that site owns.
		SkipDelete bool `json:"skipDelete"`

		// PreserveOnFailure keeps a failed run's machines. A preserved site
		// node can be logged into, because the provider writes this run's key
		// to it. A preserved tenant node cannot: nothing puts a key there and
		// the image locks the account, so the serial console, which carries
		// cloud-init's output, is the only way in.
		PreserveOnFailure bool `json:"preserveOnFailure"`

		ReuseCluster string `json:"reuseCluster"` // non-empty -> skip create, verify this cluster
	} `json:"lifecycle"`
	// Node is what the machines an infrastructure provider creates are,
	// rather than what is built on them. It is separate from Site because
	// the same node shape is used for a site controller and for the tenant
	// nodes it goes on to provision.
	Node struct {
		// User is the account the OS image bakes, and the login every
		// session uses. Empty is what the OSS os-image releases bake;
		// images from the internal pipeline bake dgxc-admin. It has to
		// match the image named by -e2e.versions os-image, or cloud-init
		// writes authorized_keys for an account that does not exist.
		User string `json:"user"`
	} `json:"node"`

	// Site configures the lane that builds its own site controller. It holds
	// identifiers and paths only: the Forge API key and the registry token
	// are read from the environment and never written to a file.
	Site Site `json:"site"`
	// Runner is what the machine running the suite supplies.
	Runner Runner `json:"runner"`
	// Nvkectl is the CLI the bootstrap runs.
	Nvkectl Nvkectl `json:"nvkectl"`
}

// Runner is the machine the suite runs from, as distinct from the site it
// reaches. Nothing here says what is under test, only where this particular
// runner keeps it, which is where a CI bot and a laptop differ.
type Runner struct {
	// TeleportIdentityFile is a tsh identity file, as a CI bot has instead
	// of a login session. Empty leaves it to tsh: $TELEPORT_IDENTITY_FILE,
	// else the runner's tsh profile.
	TeleportIdentityFile string `json:"teleportIdentityFile"`
}

// Nvkectl is the CLI the bootstrap runs on the node. Its own section rather
// than a field under runner or site: it is neither the machine the suite runs
// from nor the site it builds, and what the bootstrap asks of it will grow.
type Nvkectl struct {
	// Binary is a linux nvkectl for the node's architecture, on this machine.
	// Empty is what `gmake -C cli build-cross` writes for that architecture.
	Binary string `json:"binary"`
}

// Site is where a site controller's artifacts come from. Only that: the run's
// identity and the bootstrap's shape are test code, component versions are
// -e2e.versions keys, and the jump host is the site's, declared in
// environments/<name>.yaml.
type Site struct {
	// DeployPathRef is the commit the deploy path is exported from. Empty
	// follows the controller version: the commit itself when the controller
	// was built from one, else its controller/<version> release tag. The
	// deploy path carries the controller's own chart, so the two have to
	// agree; override only when developing the deploy path itself.
	DeployPathRef string `json:"deployPathRef"`

	// Images are the site's images and the archives staged into the node's
	// containerd, which is the cluster's, so they resolve with IfNotPresent
	// and need no pull credential.
	Images SiteImages `json:"images"`
}

// SiteImages names each image the site stages. Every field is required by
// the time a bootstrap runs, and each one's reference and archive are stated
// together so nothing can list an archive without the reference it carries.
type SiteImages struct {
	// Controller's tag is the controller version under qualification, so it
	// takes none here.
	Controller SiteImage `json:"controller"`
	// CAPIProvisioned's tag is the capi-provisioned -e2e.versions key.
	CAPIProvisioned SiteImage `json:"capi-provisioned"`
	// Profiles is built from controller/test/fixtures and published nowhere,
	// so it has no version key and its tag is here.
	Profiles SiteImage `json:"profiles"`
}

// SiteImage is an image by repository, and the docker save archive of it.
type SiteImage struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	// Archive is a runner path; a relative one resolves against the clone
	// the suite runs from.
	Archive string `json:"archive"`
}

const (
	// DefaultSiteInputs is where `gmake site-inputs` writes the archives
	// unless told otherwise, and so where the archives default to.
	DefaultSiteInputs = "~/.cache/nke-e2e/site"
)

func (i SiteImages) validate() error {
	var problems []string
	for _, image := range []struct {
		name  string
		image SiteImage
		tag   bool // whether the tag comes from this file
	}{
		{"controller", i.Controller, false},
		{"capi-provisioned", i.CAPIProvisioned, false},
		{"profiles", i.Profiles, true},
	} {
		if image.image.Repository == "" || image.image.Archive == "" {
			problems = append(problems, image.name+" needs a repository and an archive")
		}
		switch {
		case image.tag && image.image.Tag == "":
			problems = append(problems, image.name+" needs a tag")
		case !image.tag && image.image.Tag != "":
			problems = append(problems, fmt.Sprintf("%s takes its tag from -e2e.versions=%s=, not from a tag here", image.name, image.name))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("site.images: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Load reads the config file at path and applies defaults. A missing file is
// not an error; in that case all defaults apply.
func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Strict, as the environment file is parsed: an unknown key is
		// otherwise dropped in silence and the run proceeds on the default,
		// which surfaces half an hour into bare metal as behavior nobody
		// asked for, over a file that reads correctly.
		//
		// Not case-sensitive, though. This decodes through encoding/json,
		// whose field matching is not, so jumphost still binds to jumpHost.
		// That is a match rather than a miss, so nothing is lost.
		if uerr := yaml.UnmarshalStrict(data, &cfg); uerr != nil {
			return Config{}, fmt.Errorf("parsing config %s: %w", path, uerr)
		}
	case os.IsNotExist(err):
		// Missing file -> defaults only.
	default:
		return Config{}, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.Site.Images.validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	setDefault(&c.Cluster.CAPDNodeImage, "cwe-capd-kindest-node:local")

	// The same repositories and archive names `gmake site-inputs` defaults
	// to, so a local run after it needs no site block at all.
	images := &c.Site.Images
	setDefault(&images.Controller.Repository, "ghcr.io/dsx-ai-factory/nvke/nvke-controller")
	setDefault(&images.Controller.Archive, DefaultSiteInputs+"/controller.tar")
	setDefault(&images.CAPIProvisioned.Repository, "ghcr.io/dsx-ai-factory/nvke/cluster-api-provider-provisioned")
	setDefault(&images.CAPIProvisioned.Archive, DefaultSiteInputs+"/capi-provisioned.tar")
	setDefault(&images.Profiles.Repository, "nke-e2e-profiles")
	setDefault(&images.Profiles.Tag, "local")
	setDefault(&images.Profiles.Archive, DefaultSiteInputs+"/profiles.tar")
}

func setDefault(field *string, def string) {
	if *field == "" {
		*field = def
	}
}
