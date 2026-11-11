package store

import (
	"fmt"
	"regexp"
	"strings"
)

// credentialWords is the key-word group of the credential shape in scripts/hooks/pre-commit (the hook is shell and
// cannot be imported, so a test checks that both still hold the same words). Here "api key" with a space also counts.
const credentialWords = `(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY)`

// secretRules are the shapes a note or fact may not hold. The names are what the error says; the text never is.
// A rule with a judge matches loosely and the judge looks at the match (or its first group) to cut false positives.
var secretRules = []struct {
	name  string
	re    *regexp.Regexp
	judge func(string) bool
}{
	// The key word must end the identifier (so "tokens:" and "tokenizer:" pass); a value is a secret only if it has a digit
	// and, when it looks like a path or a domain, also mixed case. This lets vault entry names, paths and placeholders through.
	{"credential assignment", regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])` + strings.Replace(credentialWords, "API_?KEY", "API[_ ]?KEY", 1) +
		`(?:_[A-Za-z0-9_]*)?\s*[:=]\s*["']?([A-Za-z0-9/+_.=-]{16,})`), looksLikeSecretValue},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY( BLOCK)?-----|PuTTY-User-` + `Key-File`), nil},
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}`), nil},
	{"token prefix", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bglpat-[A-Za-z0-9_-]{20,}|\b[sr]k_(live|test)_[A-Za-z0-9]{16,}|\bxox[abprs]-[A-Za-z0-9-]{10,}|\bAKIA[0-9A-Z]{16}\b|\bAIza[0-9A-Za-z_-]{35}`), nil},
	{"token prefix", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), hasDigit}, // "sk-loading-spinner-..." is a class name
	{"web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), nil},
	{"url credentials", regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]+:([^\s/@:${}<>]{6,})@`), hasDigit},
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

// looksLikeSecretValue is true for a value that has a digit and, if it holds "/" or ".", both upper and lower case.
func looksLikeSecretValue(v string) bool {
	if !hasDigit(v) {
		return false
	}
	if strings.ContainsAny(v, "/.") {
		return v != strings.ToUpper(v) && v != strings.ToLower(v)
	}
	return true
}

// checkSecret returns an error naming the rule that matched when any of the texts looks like a credential, and records
// the refusal as an event with the rule name only. It never repeats the text. Call it before anything is written, so a
// refused secret is not on disk or in git.
func (s *Store) checkSecret(proj string, texts ...string) error {
	for _, text := range texts {
		for _, r := range secretRules {
			for _, m := range r.re.FindAllStringSubmatch(text, -1) {
				v := m[0]
				if len(m) > 1 && r.judge != nil {
					v = m[len(m)-1]
				}
				if r.judge != nil && !r.judge(v) {
					continue
				}
				s.Event("learn_refused", "", proj, r.name)
				return fmt.Errorf("refusing to store this text: it looks like a secret (rule: %s). Nothing was written. "+
					"Put the secret in the vault and store only its name, for example \"the site login is in the vault entry site-login\"", r.name)
			}
		}
	}
	return nil
}
