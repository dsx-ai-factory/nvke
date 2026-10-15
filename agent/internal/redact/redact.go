// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package redact removes the credentials a kubeadm join or init carries from the
// copies of its inputs and output that the agent logs or reports.
//
// Redaction works on copies. The agent still executes, and parses, the originals.
//
// A Credentials value holds the secrets known for one operation, collected from
// its structured inputs: the flags of a join command, the certificate key field,
// and the credential keys of a parsed kubeadm configuration. Redacting replaces
// exactly those values, so hostnames, paths, CA hashes and the rest of the text are
// untouched. The credentials kubeadm generates, which are not in any input, are
// found by their context: the "Using token:" and "Using certificate key:" lines it
// prints and the flags of the join commands it prints.
package redact

import (
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Redacted replaces a credential in a logged value.
const Redacted = "<redacted>"

// minKnownLength is the shortest known value that is replaced wherever it appears.
// Real credentials are far longer, and replacing a short value would mangle
// unrelated text. Values after a credential flag are always redacted.
const minKnownLength = 8

var (
	// tokenFlagNames are the kubeadm join flags whose value is a bootstrap token.
	// --token-ttl and --discovery-token-ca-cert-hash are not credentials.
	tokenFlagNames = map[string]bool{"--token": true, "--discovery-token": true, "--tls-bootstrap-token": true}

	tokenFlag          = regexp.MustCompile(`(--(?:token|discovery-token|tls-bootstrap-token)(?:=|\s+))(\S+)`)
	certificateKeyFlag = regexp.MustCompile(`(--certificate-key(?:=|\s+))(\S+)`)

	// usingToken and usingCertificateKey match the lines kubeadm init and
	// upload-certs print the credentials they generate on.
	usingToken          = regexp.MustCompile(`(Using token:\s*)([a-z0-9]{6}\.[a-z0-9]{16})`)
	usingCertificateKey = regexp.MustCompile(`(Using certificate key:\s*)[0-9a-fA-F]{64}`)

	// tokenShape is a bootstrap token: a 6 character ID, a dot and a 16 character secret.
	tokenShape = regexp.MustCompile(`^([a-z0-9]{6})\.[a-z0-9]{16}$`)

	// documentSeparator splits a multi-document YAML configuration.
	documentSeparator = regexp.MustCompile(`(?m)^---[ \t]*$`)

	// credentialLine matches a credential key and its value on one line of YAML
	// that could not be parsed. The values are not empty and do not span lines.
	credentialLine = regexp.MustCompile(`(?m)^([ \t]*(?:-[ \t]+)?(?:token|tlsBootstrapToken|certificateKey)[ \t]*:[ \t]*["']?)([^\s"'#]+)`)
)

// credentialKeys are the keys of a kubeadm configuration that hold a credential,
// and whether each is a bootstrap token.
var credentialKeys = map[string]bool{"token": true, "tlsBootstrapToken": true, "certificateKey": false}

// Credentials is the set of secrets known for one operation. The zero value, and a
// nil pointer, are empty and still redact by context. It is not safe for
// concurrent use.
type Credentials struct {
	replacements map[string]string
}

// New returns an empty set of credentials.
func New() *Credentials { return &Credentials{} }

// AddToken records a bootstrap token. Its ID is not secret and is kept.
func (c *Credentials) AddToken(token string) {
	c.add(token, redactToken(token))
}

// AddSecret records a secret that is replaced whole, such as a certificate key.
func (c *Credentials) AddSecret(secret string) {
	c.add(secret, Redacted)
}

func (c *Credentials) add(value, replacement string) {
	if len(value) < minKnownLength || value == replacement {
		return
	}
	if c.replacements == nil {
		c.replacements = map[string]string{}
	}
	c.replacements[value] = replacement
}

// AddArgs records the values of the credential flags in args, in the "--flag value"
// and "--flag=value" forms.
func (c *Credentials) AddArgs(args []string) {
	for i, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		if !hasValue && i+1 < len(args) {
			name, value = arg, args[i+1]
		}
		switch {
		case tokenFlagNames[name]:
			c.AddToken(value)
		case name == "--certificate-key":
			c.AddSecret(value)
		}
	}
}

// AddJoinString records the credentials in a kubeadm join command.
func (c *Credentials) AddJoinString(joinString string) {
	c.AddArgs(strings.Fields(joinString))
}

// AddConfig records the credentials in a kubeadm configuration, which can hold
// several YAML documents. Each document is parsed, so any YAML representation of
// a credential key is found. A document that does not parse is searched line by
// line instead.
func (c *Credentials) AddConfig(config string) {
	for _, doc := range documentSeparator.Split(config, -1) {
		var parsed any
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			for _, m := range credentialLine.FindAllStringSubmatch(doc, -1) {
				c.AddSecret(m[2])
			}
			continue
		}
		c.addFromParsed(parsed)
	}
}

func (c *Credentials) addFromParsed(node any) {
	switch n := node.(type) {
	case map[string]any:
		for key, value := range n {
			if isToken, ok := credentialKeys[key]; ok {
				if s, isString := value.(string); isString {
					if isToken {
						c.AddToken(s)
					} else {
						c.AddSecret(s)
					}
					continue
				}
			}
			c.addFromParsed(value)
		}
	case []any:
		for _, item := range n {
			c.addFromParsed(item)
		}
	}
}

// String returns s with the known credentials, and the credentials its context
// gives away, redacted. Everything else in s is unchanged.
func (c *Credentials) String(s string) string {
	s = redactFlag(s, tokenFlag, redactToken)
	s = redactFlag(s, certificateKeyFlag, func(string) string { return Redacted })
	s = usingToken.ReplaceAllStringFunc(s, func(m string) string {
		g := usingToken.FindStringSubmatch(m)
		return g[1] + redactToken(g[2])
	})
	s = usingCertificateKey.ReplaceAllString(s, "${1}"+Redacted)
	if c == nil {
		return s
	}
	// Longest first, so a value that contains another is replaced whole.
	known := make([]string, 0, len(c.replacements))
	for value := range c.replacements {
		known = append(known, value)
	}
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	for _, value := range known {
		s = strings.ReplaceAll(s, value, c.replacements[value])
	}
	return s
}

// Args returns a copy of args with the values of the credential flags redacted. The
// input is not modified.
func (c *Credentials) Args(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = c.String(arg)
	}
	// A flag and its value are separate arguments, so the contextual match above
	// does not see the pair.
	for i, arg := range args {
		if i+1 >= len(args) {
			break
		}
		switch {
		case tokenFlagNames[arg]:
			out[i+1] = redactToken(args[i+1])
		case arg == "--certificate-key":
			out[i+1] = Redacted
		}
	}
	return out
}

// Args returns a copy of args with the values of the credential flags redacted.
func Args(args []string) []string {
	c := New()
	c.AddArgs(args)
	return c.Args(args)
}

// Output returns the output of a kubeadm command with the credentials it prints
// redacted.
func Output(output string) string {
	return (*Credentials)(nil).String(output)
}

// redactToken returns token with its secret part replaced and its ID kept, or the
// whole value replaced if it is not a bootstrap token. A redacted value is left as
// it is.
func redactToken(token string) string {
	if m := tokenShape.FindStringSubmatch(token); m != nil {
		return m[1] + "." + Redacted
	}
	if token == Redacted || strings.HasSuffix(token, "."+Redacted) {
		return token
	}
	return Redacted
}

// redactFlag replaces the value in each match of re, which has the flag and its
// separator as its first group and the value as its second.
func redactFlag(s string, re *regexp.Regexp, redact func(string) string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		groups := re.FindStringSubmatch(match)
		return groups[1] + redact(groups[2])
	})
}
