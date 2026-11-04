// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package stackconfig loads the controller stack install's inputs: the stack
// config file, which names the NICo or LaunchLayer API and the optional image
// overrides and mirror, and the credentials, which come from the environment.
// The site spec
// holds the facts the bootstrap owns, such as the site name, the control-plane
// VIP and the SPIRE trust domain; the stack config holds what only the stack
// reads. deploy/examples/stack-config.json is the operator's starting point.
package stackconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	kjson "sigs.k8s.io/json"

	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
)

// Config is the stack config file.
type Config struct {
	// Nico is the NICo API CAPNICo calls.
	Nico Nico `json:"nico"`
	// LaunchLayer is the LaunchLayer API CAPLL calls.
	LaunchLayer LaunchLayer `json:"launchLayer"`
	// Images overrides the controller and profiles images, which default to
	// the release's artifacts.
	Images Images `json:"images"`
	// Mirror is a registry the external images are pulled from instead of
	// their upstream registries, as host[:port][/prefix], no scheme.
	Mirror string `json:"mirror"`

	// path is the absolute path of the file, set by Load and by nothing else.
	path string
	// hasNico and hasLaunchLayer record which provider objects the file holds.
	hasNico, hasLaunchLayer bool
}

// Nico is the NICo API CAPNICo calls.
type Nico struct {
	Endpoint string `json:"endpoint"`
	OrgID    string `json:"orgID"`
	APIName  string `json:"apiName"`
}

// LaunchLayer is the LaunchLayer API CAPLL calls, and the OAuth2 client it
// calls it as. The client secret comes from EnvLaunchLayerClientSecret.
type LaunchLayer struct {
	// BaseURL is the address every LaunchLayer service resolves against.
	BaseURL string `json:"baseURL"`
	// VPCURL, ComputeURL and SGURL override BaseURL for one service.
	VPCURL     string `json:"vpcURL"`
	ComputeURL string `json:"computeURL"`
	SGURL      string `json:"sgURL"`
	IssuerURL  string `json:"issuerURL"`
	ClientID   string `json:"clientID"`
	// Scopes is space-separated.
	Scopes string `json:"scopes"`
}

// Images overrides images that default to the release's artifacts.
type Images struct {
	Controller string `json:"controller"`
	Profiles   string `json:"profiles"`
}

// placeholderPrefix marks a value the example leaves for the operator to
// fill in.
const placeholderPrefix = "REPLACE_"

// objectKeys are the top-level keys that hold an object. UnmarshalStrict
// reads null into an empty object, so null is refused by name.
var objectKeys = []string{"nico", "launchLayer", "images"}

// field is one value of the file with its JSON path.
type field struct {
	path     string
	value    string
	required bool
	// template marks a value the deploy path's templates carry inside a
	// quoted scalar, which can hold no quote, backslash or control character.
	template bool
}

// usesNico reports whether the config names the NICo API: it holds a nico
// object, or no launchLayer object, which is every stack config written before
// LaunchLayer.
func (c *Config) usesNico() bool { return c.hasNico || !c.hasLaunchLayer }

// usesLaunchLayer reports whether the config holds a launchLayer object.
func (c *Config) usesLaunchLayer() bool { return c.hasLaunchLayer }

// fields lists every value in file order.
func (c *Config) fields() []field {
	nico, launchLayer := c.usesNico(), c.usesLaunchLayer()
	return []field{
		{path: "nico.endpoint", value: c.Nico.Endpoint, required: nico, template: true},
		{path: "nico.orgID", value: c.Nico.OrgID, required: nico, template: true},
		{path: "nico.apiName", value: c.Nico.APIName, required: nico, template: true},
		{path: "launchLayer.baseURL", value: c.LaunchLayer.BaseURL, required: launchLayer, template: true},
		{path: "launchLayer.vpcURL", value: c.LaunchLayer.VPCURL, template: true},
		{path: "launchLayer.computeURL", value: c.LaunchLayer.ComputeURL, template: true},
		{path: "launchLayer.sgURL", value: c.LaunchLayer.SGURL, template: true},
		{path: "launchLayer.issuerURL", value: c.LaunchLayer.IssuerURL, required: launchLayer, template: true},
		{path: "launchLayer.clientID", value: c.LaunchLayer.ClientID, required: launchLayer, template: true},
		{path: "launchLayer.scopes", value: c.LaunchLayer.Scopes, template: true},
		{path: "images.controller", value: c.Images.Controller},
		{path: "images.profiles", value: c.Images.Profiles},
		{path: "mirror", value: c.Mirror},
	}
}

// Load reads the stack config file at path and returns it validated. The
// file must be one JSON object; an unknown, duplicate or differently cased
// key is refused, as is null for an object. No value may still be a
// REPLACE_* placeholder, every required value must be set, the NICo endpoint
// and every LaunchLayer URL must be an http or https URL with no user or
// password, and the mirror must carry no scheme. The nico values are required
// unless the file holds a launchLayer object and no nico object; the
// launchLayer values are required when it holds a launchLayer object. Load
// reads nothing but the file.
//
// Every problem is reported at once, one per line as "<json path>:
// <problem>" with no file name in front; a problem with the file as a
// whole, such as an empty file, is a sentence on its own. The caller names
// the file. The credentials are not Load's: see CredentialsFromEnv.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c := &Config{path: abs}
	problems, valuesRead := c.decode(data)
	// A stray or duplicate key leaves the values in place, so they are
	// checked in the same pass; a file that is not the right shape is not.
	if valuesRead {
		problems = append(problems, c.problems()...)
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return c, nil
}

// Path returns the absolute path of the file the config was loaded from.
// It is empty for a Config that Load did not build.
func (c *Config) Path() string { return c.path }

// decode reads data into c and returns the problems with its shape: not
// one JSON object, a key the schema does not have, a duplicate key, a
// value of the wrong type, null where an object belongs. valuesRead
// reports that the values were read and are worth checking: the file was
// an object of the right types with no null object, whatever stray keys
// it carried.
func (c *Config) decode(data []byte) (problems []string, valuesRead bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return []string{"the file is empty"}, false
	}
	if trimmed[0] != '{' {
		return []string{"the file does not hold a JSON object"}, false
	}
	strictErrs, err := kjson.UnmarshalStrict(data, c)
	if err != nil {
		return []string{strings.TrimPrefix(err.Error(), "json: ")}, false
	}
	for _, err := range strictErrs {
		problems = append(problems, strictProblem(err))
	}
	// The strict decoder reads null into an empty object, which would be
	// reported as every field of it missing. Name the null instead.
	valuesRead = true
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) == nil {
		for _, key := range objectKeys {
			if value, ok := raw[key]; ok && string(bytes.TrimSpace(value)) == "null" {
				problems = append(problems, key+": must be an object, not null")
				valuesRead = false
			}
		}
		_, c.hasNico = raw["nico"]
		_, c.hasLaunchLayer = raw["launchLayer"]
	}
	return problems, valuesRead
}

// strictProblem rewrites a strict decoding error, `unknown field "a.b"`
// or `duplicate field "a.b"`, as "<json path>: <problem>".
func strictProblem(err error) string {
	var fieldErr interface{ FieldPath() string }
	kind, _, quoted := strings.Cut(err.Error(), " \"")
	if !quoted || !errors.As(err, &fieldErr) {
		return err.Error()
	}
	return fieldErr.FieldPath() + ": " + kind
}

// problems returns every problem with the values: placeholders first, then
// the required and well-formedness checks. A value that is still a
// placeholder is not a value and is skipped by the later checks.
func (c *Config) problems() []string {
	var problems []string
	for _, f := range c.fields() {
		if strings.HasPrefix(f.value, placeholderPrefix) {
			problems = append(problems, f.path+": still a REPLACE_* placeholder; fill it in")
		}
	}
	for _, f := range c.fields() {
		if strings.HasPrefix(f.value, placeholderPrefix) {
			continue
		}
		switch {
		case f.value == "":
			if f.required {
				problems = append(problems, f.path+": must be set")
			}
		case f.template && !TemplateSafe(f.value):
			problems = append(problems, f.path+": the value holds a character the templates cannot carry (a quote, a backslash or a control character)")
		}
	}
	const nicoCredential = "the token comes from " + EnvNicoToken
	const launchLayerCredential = "the client secret comes from " + EnvLaunchLayerClientSecret
	for _, u := range []struct{ path, value, credential string }{
		{"nico.endpoint", c.Nico.Endpoint, nicoCredential},
		{"launchLayer.baseURL", c.LaunchLayer.BaseURL, launchLayerCredential},
		{"launchLayer.vpcURL", c.LaunchLayer.VPCURL, launchLayerCredential},
		{"launchLayer.computeURL", c.LaunchLayer.ComputeURL, launchLayerCredential},
		{"launchLayer.sgURL", c.LaunchLayer.SGURL, launchLayerCredential},
		{"launchLayer.issuerURL", c.LaunchLayer.IssuerURL, launchLayerCredential},
	} {
		if problem := urlProblem(u.path, u.value, u.credential); problem != "" {
			problems = append(problems, problem)
		}
	}
	if c.Mirror != "" && strings.Contains(c.Mirror, "://") {
		problems = append(problems, fmt.Sprintf("mirror: %q is a host[:port][/prefix], not a URL", c.Mirror))
	}
	return problems
}

// urlProblem returns what is wrong with the URL value at path, or "" for an
// empty value, a placeholder or an http or https URL with a host and no
// userinfo. credential says where the credential comes from instead.
func urlProblem(path, value, credential string) string {
	if value == "" || strings.HasPrefix(value, placeholderPrefix) {
		return ""
	}
	u, err := url.Parse(value)
	switch {
	case err != nil:
		// The value is not printed: malformed userinfo fails the parse, and
		// may be a password.
		return path + ": must be an http or https URL"
	case u.User != nil:
		// The value is not printed: the userinfo may be a password.
		return path + ": must carry no user or password; " + credential
	case u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return fmt.Sprintf("%s: %q must be an http or https URL", path, value)
	}
	return ""
}

// The environment variables the controller stack install reads its
// credentials from. They stay out of the stack config so the file holds no
// secret and can be committed.
const (
	EnvNicoToken        = "NVKECTL_NICO_TOKEN" //nolint:gosec // G101: an env var name, not a credential
	EnvNodeUser         = "NVKECTL_NODE_USER"
	EnvNodePassword     = "NVKECTL_NODE_PASSWORD"     //nolint:gosec // G101: an env var name, not a credential
	EnvDatabasePassword = "NVKECTL_DATABASE_PASSWORD" //nolint:gosec // G101: an env var name, not a credential

	EnvLaunchLayerClientSecret = "NVKECTL_LAUNCHLAYER_CLIENT_SECRET" //nolint:gosec // G101: an env var name, not a credential
)

// Credentials are the controller stack install's credentials. NodeUser and
// NodePassword are the login rendered into every tenant node's cloud-init.
type Credentials struct {
	NicoToken               string
	NodeUser                string
	NodePassword            string
	DatabasePassword        string
	LaunchLayerClientSecret string
}

// TemplateSafe reports whether value can go into the deploy path templates,
// which carry no escaping: no quote, backslash or control character.
func TemplateSafe(value string) bool {
	return !strings.ContainsAny(value, "\"\\") && !strings.ContainsFunc(value, unicode.IsControl)
}

// CredentialsFromEnv reads the controller stack install's credentials from
// the environment, trimmed, and names every one that is unset, one problem
// per line: the node login and database password always, the NICo token when
// the config names the NICo API, and the LaunchLayer client secret when it
// names the LaunchLayer API. The provider credentials and the node login land
// in templates that carry no escaping, so a quote, a backslash or a control
// character in them is refused too. No value is ever part of the error.
func (c *Config) CredentialsFromEnv() (Credentials, error) {
	nico, launchLayer := c.usesNico(), c.usesLaunchLayer()
	s := Credentials{
		NodeUser:         strings.TrimSpace(envalias.Get(EnvNodeUser)),
		NodePassword:     strings.TrimSpace(envalias.Get(EnvNodePassword)),
		DatabasePassword: strings.TrimSpace(envalias.Get(EnvDatabasePassword)),
	}
	var envs [][2]string
	if nico {
		s.NicoToken = strings.TrimSpace(envalias.Get(EnvNicoToken))
		envs = append(envs, [2]string{EnvNicoToken, s.NicoToken})
	}
	envs = append(envs, [2]string{EnvNodeUser, s.NodeUser}, [2]string{EnvNodePassword, s.NodePassword}, [2]string{EnvDatabasePassword, s.DatabasePassword})
	if launchLayer {
		s.LaunchLayerClientSecret = strings.TrimSpace(envalias.Get(EnvLaunchLayerClientSecret))
		envs = append(envs, [2]string{EnvLaunchLayerClientSecret, s.LaunchLayerClientSecret})
	}
	templated := map[string]bool{EnvNicoToken: true, EnvNodeUser: true, EnvNodePassword: true, EnvLaunchLayerClientSecret: true}
	var problems []string
	for _, env := range envs {
		switch {
		case env[1] == "":
			problems = append(problems, env[0]+" is not set")
		case templated[env[0]] && !TemplateSafe(env[1]):
			problems = append(problems, env[0]+": the value holds a character the templates cannot carry (a quote, a backslash or a control character)")
		}
	}
	if len(problems) > 0 {
		return s, errors.New(strings.Join(problems, "\n"))
	}
	return s, nil
}
