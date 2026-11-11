package store

import (
	"fmt"
	"regexp"
)

// credentialWords is the key-word part of the credential shape in scripts/hooks/pre-commit (the hook is shell and
// cannot be imported, so a test checks that both still hold the same words).
const credentialWords = `(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY)[A-Za-z0-9_]*`

// secretRules are the shapes a note or fact may not hold. The names are what the error says; the text never is.
var secretRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"credential assignment", regexp.MustCompile(`(?i)` + credentialWords + `\s*[:=]\s*["']?[A-Za-z0-9/+_.=-]{16,}`)},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY( BLOCK)?-----|PuTTY-User-` + `Key-File`)},
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}`)},
	{"token prefix", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bxox[abprs]-[A-Za-z0-9-]{10,}|\bAKIA[0-9A-Z]{16}\b`)},
}

// refuseSecret returns an error naming the rule that matched when text looks like a credential. It never repeats the
// text. Call it before anything is written, so a refused secret is not on disk or in git.
func refuseSecret(text string) error {
	for _, r := range secretRules {
		if r.re.MatchString(text) {
			return fmt.Errorf("refusing to store this text: it looks like a secret (rule: %s). Nothing was written. "+
				"Put the secret in the vault and store only its name, for example \"the site login is in the vault entry site-login\"", r.name)
		}
	}
	return nil
}
