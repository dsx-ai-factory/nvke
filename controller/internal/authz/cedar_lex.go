// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

// stripCedarComments removes Cedar's two comment forms -- `//` to end of
// line, and `/* ... */`, both real Cedar syntax (internal/parser's
// cedar_tokenize.go scanComment, in the cedar-go dependency this package
// already imports) -- from policy source before any regexRef check scans it
// for role/action references. Without this, a line like
// `// removed NKE::Role::"admin"` reads as a real reference to a role that
// may no longer exist, and checkActionVocabulary/checkRoleVocabulary would
// reject an otherwise-valid bundle over dead comment text.
//
// String-literal-aware, not just a blind "//"/"/*" scan: Cedar string
// literals cannot contain a literal newline (scanString errors on one) but
// can contain a literal "//" or "/*" -- an action or role id like
// NKE::Action::"http://example" is valid Cedar. A blind scan would treat
// that "//" as starting a comment and silently swallow the rest of the
// line, hiding a real reference instead of finding a fake one -- trading
// Codex's false-positive for a worse false-negative. This walks the source
// tracking whether it is inside a double-quoted string (respecting Cedar's
// backslash escaping, the same rule scanEscape implements) and only treats
// "//"/"/*" as a comment opener outside of one.
//
// Independently testable: this is a pure lexer over a []byte, with no
// dependency on a bundle directory or load context -- see cedar_lex_test.go.
func stripCedarComments(source []byte) []byte {
	var out []byte
	inString := false
	for i := 0; i < len(source); i++ {
		c := source[i]

		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(source) {
				// Escaped character: copy it verbatim and skip past it so an
				// escaped quote (\") does not end the string early.
				out = append(out, source[i+1])
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}

		if c == '/' && i+1 < len(source) && source[i+1] == '/' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			if i < len(source) {
				out = append(out, '\n')
			}
			continue
		}

		if c == '/' && i+1 < len(source) && source[i+1] == '*' {
			i += 2
			for i+1 < len(source) && (source[i] != '*' || source[i+1] != '/') {
				if source[i] == '\n' {
					out = append(out, '\n')
				}
				i++
			}
			i++ // land on the closing '/'; the loop's i++ advances past it
			continue
		}

		out = append(out, c)
	}
	return out
}
