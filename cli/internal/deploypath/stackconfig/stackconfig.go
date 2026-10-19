// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package stackconfig loads the controller stack install's inputs: the stack
// config file, which names the NICo API, and the credentials, which come from
// the environment. The site spec holds the facts the bootstrap owns, such as
// the site name, the control-plane VIP and the SPIRE trust domain; the stack
// artifacts file names the images and charts; the stack config holds what
// only the stack reads. deploy/examples/stack-config.json is the operator's
// starting point.
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

	// path is the absolute path of the file, set by Load and by nothing else.
	path string
}

// Nico is the NICo API CAPNICo calls.
type Nico struct {
	Endpoint string `json:"endpoint"`
	OrgID    string `json:"orgID"`
	APIName  string `json:"apiName"`
}

// placeholderPrefix marks a value the example leaves for the operator to
// fill in.
const placeholderPrefix = "REPLACE_"

// objectKeys are the top-level keys that hold an object. UnmarshalStrict
// reads null into an empty object, so null is refused by name.
var objectKeys = []string{"nico"}

// field is one value of the file with its JSON path.
type field struct {
	path     string
	value    string
	required bool
	// template marks a value the deploy path's templates carry inside a
	// quoted scalar, which can hold no quote, backslash or control character.
	template bool
}

// fields lists every value in file order.
func (c *Config) fields() []field {
	return []field{
		{path: "nico.endpoint", value: c.Nico.Endpoint, required: true, template: true},
		{path: "nico.orgID", value: c.Nico.OrgID, required: true, template: true},
		{path: "nico.apiName", value: c.Nico.APIName, required: true, template: true},
	}
}

// Load reads the stack config file at path and returns it validated. The
// file must be one JSON object; an unknown, duplicate or differently cased
// key is refused, as is null for an object. No value may still be a
// REPLACE_* placeholder, every required value must be set, and the NICo
// endpoint must be an http or https URL with no user or password. Load reads
// nothing but the file.
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
	if ep := c.Nico.Endpoint; ep != "" && !strings.HasPrefix(ep, placeholderPrefix) {
		u, err := url.Parse(ep)
		switch {
		case err == nil && u.User != nil:
			// The value is not printed: the userinfo may be a password.
			problems = append(problems, "nico.endpoint: must carry no user or password; the token comes from "+EnvNicoToken)
		case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
			problems = append(problems, fmt.Sprintf("nico.endpoint: %q must be an http or https URL", ep))
		}
	}
	return problems
}

// The environment variables the controller stack install reads its
// credentials from. They stay out of the stack config so the file holds no
// secret and can be committed.
const (
	EnvNicoToken        = "NVKECTL_NICO_TOKEN" //nolint:gosec // G101: an env var name, not a credential
	EnvNodeUser         = "NVKECTL_NODE_USER"
	EnvNodePassword     = "NVKECTL_NODE_PASSWORD"     //nolint:gosec // G101: an env var name, not a credential
	EnvDatabasePassword = "NVKECTL_DATABASE_PASSWORD" //nolint:gosec // G101: an env var name, not a credential
)

// Credentials are the controller stack install's credentials. NodeUser and
// NodePassword are the login rendered into every tenant node's cloud-init.
type Credentials struct {
	NicoToken        string
	NodeUser         string
	NodePassword     string
	DatabasePassword string
}

// envs pairs each credential's environment variable with its value.
func (s *Credentials) envs() [][2]string {
	return [][2]string{
		{EnvNicoToken, s.NicoToken},
		{EnvNodeUser, s.NodeUser},
		{EnvNodePassword, s.NodePassword},
		{EnvDatabasePassword, s.DatabasePassword},
	}
}

// TemplateSafe reports whether value can go into the deploy path templates,
// which carry no escaping: no quote, backslash or control character.
func TemplateSafe(value string) bool {
	return !strings.ContainsAny(value, "\"\\") && !strings.ContainsFunc(value, unicode.IsControl)
}

// CredentialsFromEnv reads the controller stack install's credentials from
// the environment, trimmed, and names every one that is unset, one problem
// per line. The NICo token and the node login land in templates that carry
// no escaping, so a quote, a backslash or a control character in them is
// refused too. No value is ever part of the error.
func CredentialsFromEnv() (Credentials, error) {
	s := Credentials{
		NicoToken:        strings.TrimSpace(envalias.Get(EnvNicoToken)),
		NodeUser:         strings.TrimSpace(envalias.Get(EnvNodeUser)),
		NodePassword:     strings.TrimSpace(envalias.Get(EnvNodePassword)),
		DatabasePassword: strings.TrimSpace(envalias.Get(EnvDatabasePassword)),
	}
	templated := map[string]bool{EnvNicoToken: true, EnvNodeUser: true, EnvNodePassword: true}
	var problems []string
	for _, env := range s.envs() {
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
