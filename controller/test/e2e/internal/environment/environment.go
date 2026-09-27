// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package environment loads a declaration of infrastructure that already
// exists, so a run can be told where to execute rather than inferring it.
//
// This is deliberately not config.Config. That carries per-run knobs such as
// kubeconfig and lifecycle flags, which change run to run and default to
// something sensible when absent. An environment cannot be defaulted: a site
// and a VPC either exist or they do not, so an unresolvable one fails the run
// immediately.
//
// Only the top level is common. Almost everything an environment says is
// provider dialect, because a site, a VPC and a jump host are Forge's
// vocabulary and another provider has its own, so the block below the provider
// name is passed through undecoded for that provider to interpret.
package environment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	// Dir is where Load resolves names, relative to the suite package
	// directory that `go test` makes the working directory.
	Dir = "environments"
)

// Environment is one infrastructure declaration: who provides it, and that
// provider's description of it.
type Environment struct {
	// Name is the name Load was called with, kept for error messages.
	Name string `json:"-"`

	// Provider names the dialect Infrastructure is written in.
	Provider string `json:"provider"`

	// Infrastructure is opaque here and decoded by the named provider.
	Infrastructure json.RawMessage `json:"infrastructure"`
}

// Load resolves an environment name to environments/<name>.yaml and reads it.
//
// Callers pass a name rather than a path so the directory layout stays out of
// every caller and out of every CI invocation.
func Load(name string) (*Environment, error) {
	if name == "" {
		return nil, fmt.Errorf("no environment named: pass -e2e.environment")
	}
	// Names index a checked-in directory, so anything that could climb out of
	// it is a mistake worth naming rather than a path to follow.
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("environment name %q must be a bare name, not a path", name)
	}

	return loadPath(name, filepath.Join(Dir, name+".yaml"))
}

// loadPath is Load with the path already resolved, so a test can read the
// checked-in environments from wherever it runs.
func loadPath(name, path string) (*Environment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading environment %q from %s: %w", name, path, err)
	}

	data, err = expand(data)
	if err != nil {
		return nil, fmt.Errorf("resolving environment %q from %s: %w", name, path, err)
	}

	env := Environment{Name: name}
	if err := yaml.UnmarshalStrict(data, &env); err != nil {
		return nil, fmt.Errorf("parsing environment %s: %w", path, err)
	}
	if env.Provider == "" {
		return nil, fmt.Errorf("environment %s names no provider", path)
	}

	return &env, nil
}

// reference matches ${NAME}. Only the braced form is recognized: a bare $NAME
// would also match prose in a comment, and expansion happens before the YAML
// is parsed, so a comment is not yet a comment when this runs.
var reference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand substitutes ${NAME} from the process environment.
//
// The structure of an environment -- which sites exist, which machine shapes
// they offer and what those shapes provide -- is engineering content and lives
// in the file. The identifiers of the account it all sits in are not: they
// name one organization's real infrastructure, and this repository is the
// staging ground for a public one. So they are supplied at run time from
// repository variables, which is already how the Teleport proxy and the jump
// host reach a run.
//
// An unset variable is an error rather than an empty string. Substituting
// nothing would produce a file that parses, so the run would fail later
// against an API that was asked to create a cluster in org "" -- and the
// premise of this package is that an environment cannot be defaulted. Every
// missing name is collected, so one run reports the whole list.
func expand(data []byte) ([]byte, error) {
	missing := make(map[string]struct{})

	expanded := reference.ReplaceAllFunc(data, func(match []byte) []byte {
		name := string(reference.FindSubmatch(match)[1])
		value, ok := os.LookupEnv(name)
		if !ok {
			missing[name] = struct{}{}
			return match
		}
		return []byte(value)
	})

	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unset: %s", strings.Join(names, ", "))
	}

	return expanded, nil
}

// decodeInfrastructure decodes the opaque block into a provider's own shape. It
// is unexported because the provider accessors below are the way in: decoding
// Forge's dialect as some other provider's would otherwise be silent.
func (e *Environment) decodeInfrastructure(provider string, into any) error {
	if e.Provider != provider {
		return fmt.Errorf("environment %q is provided by %q, not %q", e.Name, e.Provider, provider)
	}
	if len(e.Infrastructure) == 0 {
		return fmt.Errorf("environment %q declares no infrastructure", e.Name)
	}
	if err := json.Unmarshal(e.Infrastructure, into); err != nil {
		return fmt.Errorf("decoding %s infrastructure in environment %q: %w", provider, e.Name, err)
	}
	return nil
}
