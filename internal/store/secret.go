package store

import (
	"fmt"
	"regexp"
	"strings"
)

// credentialWords is the key-word group of the credential shape in scripts/hooks/pre-commit (the hook is shell and
// cannot be imported, so a test checks that both still hold the same words). Here "api key" with a space also counts.
const credentialWords = `(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY)`

// The names of the credential-assignment rule. credNames is the shared key words with the _PASS suffix; saltNames are
// the WordPress key and salt constants and any _SALT suffix. Both are matched at the end of an identifier.
var (
	credNames = `(?:(?:^|[^A-Za-z0-9])` + strings.Replace(credentialWords, "API_?KEY", "API[_ ]?KEY", 1) + `|_PASS)`
	saltNames = `(?:(?:^|[^A-Za-z0-9])(?:AUTH|LOGGED_IN|NONCE)_(?:KEY|SALT)|_SALT)`
	nameTail  = `(?:_[A-Za-z0-9_]*)?`
	// Between a name and a value that opens with a quote (define('X', 'v'), "x": "v", 'x' => 'v').
	quotedSep = `(?:["']\s*,\s*|["']?\s*(?:=>|[:=])\s*)`
	// wp [--flags] config set [--flags] NAME followed by the value.
	wpSet = `(?i)\bwp\s+(?:--\S+\s+)*config\s+set\s+(?:--\S+\s+)*[A-Za-z0-9_]*`
)

// secretRules are the shapes a note or fact may not hold. The names are what the error says; the text never is.
// A rule with a judge matches loosely and the judge looks at the match (or its first group) to cut false positives.
var secretRules = []struct {
	name  string
	re    *regexp.Regexp
	judge func(string) bool
}{
	// Also the quoted forms: define('DB_PASSWORD', 'x'), "password": "x" and 'password' => 'x'.
	// The key word must end the identifier (so "tokens:" and "tokenizer:" pass); a value is a secret only if it has a digit
	// and, when it looks like a path or a domain, also mixed case. This lets vault entry names, paths and placeholders through.
	// The WordPress key and salt names count here too, and the suffixes _PASS and _SALT.
	{"credential assignment", regexp.MustCompile(`(?i)(?:` + credNames + `|` + saltNames + `)` + nameTail +
		`(?:["']\s*,\s*["']|["']?\s*(?:=>|[:=])\s*["']?)([A-Za-z0-9/+_.=-]{16,})`), looksLikeSecretValue},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY( BLOCK)?-----|PuTTY-User-` + `Key-File`), nil},
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}`), nil},
	{"token prefix", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bglpat-[A-Za-z0-9_-]{20,}|\b[sr]k_(live|test)_[A-Za-z0-9]{16,}|\bxox[abprs]-[A-Za-z0-9-]{10,}|\bAKIA[0-9A-Z]{16}\b|\bAIza[0-9A-Za-z_-]{35}`), nil},
	{"token prefix", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), hasDigit}, // "sk-loading-spinner-..." is a class name
	{"web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), nil},
	{"url credentials", regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]+:([^\s/@:${}<>]{6,})@`), hasDigit},
}

var placeholderValue = regexp.MustCompile(`^(?:\$\{?\w+\}?|<.*>|%\w+%)$`)

func init() {
	// Quoted values are taken up to the closing quote, so symbols in the value do not hide it. Needs a digit and no space
	// (or, for a key or salt name, 32 or more characters of any kind: those are random by nature, and the stock
	// placeholder 'put your unique phrase here' is shorter and has no digit). The same for the wp-cli form.
	for _, c := range []struct {
		names string
		judge func(string) bool
	}{{credNames, looksLikeQuotedSecret}, {saltNames, looksLikeSaltValue}} {
		wpNames := strings.NewReplacer("(?:^|[^A-Za-z0-9])", "").Replace(c.names)
		for _, form := range []string{
			`(?i)` + c.names + nameTail + quotedSep + `'([^']*)'`,
			`(?i)` + c.names + nameTail + quotedSep + `"([^"]*)"`,
			wpSet + wpNames + nameTail + `\s+'([^']*)'`,
			wpSet + wpNames + nameTail + `\s+"([^"]*)"`,
			wpSet + wpNames + nameTail + `\s+([^\s'"]+)`,
		} {
			secretRules = append(secretRules, struct {
				name  string
				re    *regexp.Regexp
				judge func(string) bool
			}{"credential assignment", regexp.MustCompile(form), c.judge})
		}
	}
	// wp config create --dbpass=VALUE
	for _, form := range []string{`(?i)--dbpass=\s*'([^']*)'`, `(?i)--dbpass=\s*"([^"]*)"`, `(?i)--dbpass=([^\s'"]+)`} {
		secretRules = append(secretRules, struct {
			name  string
			re    *regexp.Regexp
			judge func(string) bool
		}{"credential assignment", regexp.MustCompile(form), looksLikeQuotedSecret})
	}
}

// looksLikeQuotedSecret judges a value read up to its closing quote: 12 or more characters, no space, a digit, and the
// mixed-case rule for paths. A variable or placeholder as a whole ($NAME, ${NAME}, <...>, %NAME%) is not a value; one that only starts with such a
// character is, since random keys use all of them.
func looksLikeQuotedSecret(v string) bool {
	if len(v) < 12 || strings.ContainsAny(v, " \t") || placeholderValue.MatchString(v) || strings.Contains(v, "getenv") {
		return false
	}
	return looksLikeSecretValue(v)
}

// looksLikeSaltValue is true for a key or salt value of 32 or more characters, or for a shorter one that passes the
// password judge. A real salt is 64 random characters; a placeholder phrase is shorter.
func looksLikeSaltValue(v string) bool {
	return (len(v) >= 32 && !placeholderValue.MatchString(v)) || looksLikeQuotedSecret(v)
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
