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
// prints and the flags of the join commands it prints. Text the agent saved
// earlier, such as its logs, is redacted with Saved, which also matches by shape
// and field name.
package redact

import (
	"bufio"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v2"
)

// Redacted is what replaces a credential in a logged value.
const Redacted = "<redacted>"

// minKnownLength is the shortest known value that is replaced wherever it appears.
// Real credentials are far longer, and replacing a short value would mangle
// unrelated text. Values after a credential flag are always redacted.
const minKnownLength = 8

const (
	// tokenPattern is a bootstrap token: a 6 character ID, a dot and a 16 character secret.
	tokenPattern = `[a-z0-9]{6}\.[a-z0-9]{16}`

	// certificateKeyPattern is a certificate key: 64 hex digits.
	certificateKeyPattern = `[0-9a-fA-F]{64}`

	// fieldQuote is a quote, escaped any number of times.
	fieldQuote = `\\*["']`

	// flagSeparator follows a credential flag: "=", spaces, or "," in a JSON array,
	// whose quotes can be escaped.
	flagSeparator = `(?:=|[ \t]+|` + fieldQuote + `\s*,\s*` + fieldQuote + `)`

	// fieldSeparator follows a field name: an optional quote, then ":" or "=".
	fieldSeparator = `(?:` + fieldQuote + `)?[ \t]*[:=]`

	// usingCertificateKeyLabel is what kubeadm prints on the line before a certificate key.
	usingCertificateKeyLabel = "Using certificate key:"
)

var (
	// tokenFlagNames are the kubeadm join flags whose value is a bootstrap token.
	// --token-ttl and --discovery-token-ca-cert-hash are not credentials.
	tokenFlagNames = map[string]bool{"--token": true, "--discovery-token": true, "--tls-bootstrap-token": true}

	// tokenFlag and certificateKeyFlag match a flag and a value of the shape kubeadm
	// generates. Matching the shape, and not any word after the flag, leaves prose
	// that names a flag alone, as in "flag --token is required". Values that were
	// given to the agent are known from its inputs, and are replaced whatever their shape.
	tokenFlag          = regexp.MustCompile(`(?m)((?:^|[\s\["'(])--(?:token|discovery-token|tls-bootstrap-token)` + flagSeparator + `)(` + tokenPattern + `)`)
	certificateKeyFlag = regexp.MustCompile(`(?m)((?:^|[\s\["'(])--certificate-key` + flagSeparator + `)(` + certificateKeyPattern + `)`)

	// usingToken and usingCertificateKey match the lines kubeadm init and
	// upload-certs print. The certificate key is on the next line.
	usingToken          = regexp.MustCompile(`(Using token:\s*)(` + tokenPattern + `)`)
	usingCertificateKey = regexp.MustCompile(`(` + usingCertificateKeyLabel + `(?:\s|\\[nrt])*)` + certificateKeyPattern)

	// tokenShape matches a value that is a bootstrap token.
	tokenShape = regexp.MustCompile(`^` + tokenPattern + `$`)

	// tokenCandidate is a bootstrap token's shape anywhere in text.
	tokenCandidate = regexp.MustCompile(tokenPattern)

	// certificateKeyField matches a certificate key after its YAML, JSON or Go field name.
	certificateKeyField = regexp.MustCompile(`((?:certificateKey|certificate_key|CertificateKey)` + fieldSeparator + `[ \t]*(?:` + fieldQuote + `)?)` + certificateKeyPattern + `\b`)

	// encryptionKeyField matches the etcd encryption key after its proto, JSON or Go
	// field name. An unquoted value must start right after the separator.
	encryptionKeyField = regexp.MustCompile(`((?:api_encryption_key|apiEncryptionKey|ApiEncryptionKey)` + fieldSeparator + `(?:[ \t]*` + fieldQuote + `)?)[^\s"'\\,}]+`)

	// quotedScalar matches a double or single quoted YAML scalar, which can write a
	// character as an escape sequence.
	quotedScalar = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^']|'')*'`)

	// credentialLine matches a credential key, in block or flow style, and its value
	// in YAML that could not be parsed. The value is not empty and ends at a space or
	// at the end of a flow collection.
	credentialLine = regexp.MustCompile(`(?m)(?:^|[\s{,])[ \t]*(?:-[ \t]+)?(token|tlsBootstrapToken|certificateKey)[ \t]*:[ \t]*["']?([^\s"'#,}\]]+)`)
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
func (credentials *Credentials) AddToken(token string) {
	credentials.add(token, redactToken(token))
}

// AddSecret records a secret that is replaced whole, such as a certificate key.
func (credentials *Credentials) AddSecret(secret string) {
	credentials.add(secret, Redacted)
}

func (credentials *Credentials) add(value, replacement string) {
	if len(value) < minKnownLength || value == replacement {
		return
	}
	if credentials.replacements == nil {
		credentials.replacements = map[string]string{}
	}
	credentials.replacements[value] = replacement
}

// AddArgs records the values of the credential flags in args, in the "--flag value"
// and "--flag=value" forms.
func (credentials *Credentials) AddArgs(args []string) {
	for i, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		if !hasValue && i+1 < len(args) {
			name, value = arg, args[i+1]
		}
		switch {
		case tokenFlagNames[name]:
			credentials.AddToken(value)
		case name == "--certificate-key":
			credentials.AddSecret(value)
		}
	}
}

// AddJoinString records the credentials in a kubeadm join command.
func (credentials *Credentials) AddJoinString(joinString string) {
	credentials.AddArgs(strings.Fields(joinString))
}

// AddConfig records the credentials in a kubeadm configuration, which can hold
// several YAML documents. Each document is parsed, so any YAML representation of
// a credential key is found. A configuration that does not parse is searched line
// by line instead.
func (credentials *Credentials) AddConfig(config string) {
	decoder := yaml.NewDecoder(strings.NewReader(config))
	for {
		var doc any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			for _, match := range credentialLine.FindAllStringSubmatch(config, -1) {
				credentials.addCredential(match[1], match[2])
			}
			break
		}
		credentials.addFromParsed(doc)
	}
	credentials.addWrittenForms(config)
}

// addWrittenForms records the way each known credential is written in config when
// that differs from its value. Parsing decodes escape sequences, such as \u0061 for
// "a", but the text that is logged still has them.
func (credentials *Credentials) addWrittenForms(config string) {
	if len(credentials.replacements) == 0 {
		return
	}
	for _, scalar := range quotedScalar.FindAllString(config, -1) {
		var decoded string
		if err := yaml.Unmarshal([]byte(scalar), &decoded); err != nil {
			continue
		}
		replacement, known := credentials.replacements[decoded]
		if !known {
			continue
		}
		credentials.add(scalar[1:len(scalar)-1], replacement)
	}
}

// isCredentialKey reports whether key is a key of a kubeadm configuration that
// holds a credential.
func isCredentialKey(key string) bool {
	_, ok := credentialKeys[key]
	return ok
}

// addCredential records value, held by the configuration key key, as a token or as
// a secret that is replaced whole.
func (credentials *Credentials) addCredential(key, value string) {
	if credentialKeys[key] {
		credentials.AddToken(value)
		return
	}
	credentials.AddSecret(value)
}

func (credentials *Credentials) addFromParsed(node any) {
	switch typed := node.(type) {
	case map[any]any:
		for key, value := range typed {
			name, _ := key.(string)
			if text, isString := value.(string); isString && isCredentialKey(name) {
				credentials.addCredential(name, text)
				continue
			}
			credentials.addFromParsed(value)
		}
	case []any:
		for _, item := range typed {
			credentials.addFromParsed(item)
		}
	}
}

// String returns text with the known credentials, and the credentials its context
// gives away, redacted. Everything else in text is unchanged.
func (credentials *Credentials) String(text string) string {
	text = redactFlag(text, tokenFlag, redactToken)
	text = redactFlag(text, certificateKeyFlag, func(string) string { return Redacted })
	text = usingToken.ReplaceAllStringFunc(text, func(match string) string {
		groups := usingToken.FindStringSubmatch(match)
		return groups[1] + redactToken(groups[2])
	})
	text = usingCertificateKey.ReplaceAllString(text, "${1}"+Redacted)
	if credentials == nil {
		return text
	}
	// Longest first, so a value that contains another is replaced whole.
	known := make([]string, 0, len(credentials.replacements))
	for value := range credentials.replacements {
		known = append(known, value)
	}
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	for _, value := range known {
		text = strings.ReplaceAll(text, value, credentials.replacements[value])
	}
	return text
}

// Saved redacts text the agent saved earlier, such as its logs. Besides what String
// redacts, it redacts bootstrap tokens by shape, keeping the ID, and certificate and
// etcd encryption keys by field name.
func (credentials *Credentials) Saved(text string) string {
	text = credentials.String(text)
	text = certificateKeyField.ReplaceAllString(text, "${1}"+Redacted)
	text = encryptionKeyField.ReplaceAllString(text, "${1}"+Redacted)
	return redactTokenShapes(text)
}

// CopySaved writes src to dst with Saved applied line by line. A "Using certificate
// key:" line is joined with the line holding the key.
func (credentials *Credentials) CopySaved(dst io.Writer, src io.Reader) error {
	reader := bufio.NewReader(src)
	writer := bufio.NewWriter(dst)
	for {
		line, err := reader.ReadString('\n')
		for err == nil && strings.HasSuffix(strings.TrimRight(line, " \t\r\n"), usingCertificateKeyLabel) {
			var next string
			next, err = reader.ReadString('\n')
			line += next
		}
		if _, writeErr := writer.WriteString(credentials.Saved(line)); writeErr != nil {
			return writeErr
		}
		if errors.Is(err, io.EOF) {
			return writer.Flush()
		}
		if err != nil {
			return err
		}
	}
}

// Args returns a copy of args with each argument redacted as Saved does, and the
// values of the credential flags redacted. The input is not modified.
func (credentials *Credentials) Args(args []string) []string {
	redactedArgs := make([]string, len(args))
	for i, arg := range args {
		redactedArgs[i] = credentials.Saved(arg)
	}
	// A flag and its value are separate arguments, so the contextual match above
	// does not see the pair.
	for i, arg := range args {
		if i+1 >= len(args) {
			break
		}
		switch {
		case tokenFlagNames[arg]:
			redactedArgs[i+1] = redactToken(args[i+1])
		case arg == "--certificate-key":
			redactedArgs[i+1] = Redacted
		}
	}
	return redactedArgs
}

// Args returns a copy of args with credentials redacted, as Credentials.Args does.
func Args(args []string) []string {
	credentials := New()
	credentials.AddArgs(args)
	return credentials.Args(args)
}

// redactToken returns token with its secret part replaced and its ID kept, or the
// whole value replaced if it is not a bootstrap token. A redacted value is left as
// it is.
func redactToken(token string) string {
	if tokenShape.MatchString(token) {
		id, _, _ := strings.Cut(token, ".")
		return id + "." + Redacted
	}
	if token == Redacted || strings.HasSuffix(token, "."+Redacted) {
		return token
	}
	return Redacted
}

// redactFlag replaces the value in each match of pattern, which has the flag and its
// separator as its first group and the value as its second.
func redactFlag(text string, pattern *regexp.Regexp, redactValue func(string) string) string {
	return pattern.ReplaceAllStringFunc(text, func(match string) string {
		groups := pattern.FindStringSubmatch(match)
		return groups[1] + redactValue(groups[2])
	})
}

// redactTokenShapes redacts the secret of each bootstrap token that stands alone,
// keeping its ID.
func redactTokenShapes(text string) string {
	var out strings.Builder
	last := 0
	for _, match := range tokenCandidate.FindAllStringIndex(text, -1) {
		start, end := match[0], match[1]
		if !standsAlone(text, start, end) {
			continue
		}
		idEnd := start + strings.IndexByte(text[start:end], '.')
		out.WriteString(text[last:idEnd])
		out.WriteString("." + Redacted)
		last = end
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

// standsAlone reports whether no letter, digit or dot is next to text[start:end],
// ignoring a JSON escape before it.
func standsAlone(text string, start, end int) bool {
	escaped := start >= 2 && text[start-2] == '\\' && strings.IndexByte("nrt", text[start-1]) >= 0
	before := start == 0 || !isTokenByte(text[start-1]) || escaped
	after := end == len(text) || !isTokenByte(text[end])
	return before && after
}

// isTokenByte reports whether c is an ASCII letter, a digit or a dot.
func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.'
}
