package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The samples are joined from pieces so this file does not trip the repository's own leak check.
var secretSamples = map[string]string{
	"credential assignment":        "the site login is set with API" + "_KEY=" + "abcd1234efgh5678ijkl",
	"token prefix #gl":             "ci uses glp" + "at-abcdefghij0123456789",
	"token prefix #ghpat":          "github_p" + "at_abcdefghij0123456789ABCD",
	"token prefix #live":           "billing sk" + "_live_abcdefghij0123456789",
	"token prefix #aiza":           "maps AI" + "zaSyA-abcdefghij0123456789abcdefghijk",
	"credential assignment #space": "the api k" + "ey: abcd1234efgh5678ijkl",
	"credential assignment #url":   "call https://example.invalid/cb?tok" + "en=Ab3dEf6hIj9lMn2pQr5tUv",
	"credential assignment #b64":   "AWS_SECRET_ACCESS" + "_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYzzzzzzzzzz",
	"web token":                    "session eyJ" + "hbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkw.abcdefghijKLMNOP",
	"url credentials":              "db at postgres://app:" + "s3cretPass1@db.example.invalid/main",
	"private key block":            "key follows: -----BEGIN RSA PRIVATE" + " KEY----- MIIEow",
	"bearer token":                 "send header Authorization: Bearer " + "abcdefghij0123456789ABCDEF",
	"token prefix":                 "use sk" + "-abcdefghij0123456789abcd for the service",
	"token prefix #gh":             "pushed with gh" + "p_abcdefghij0123456789abcdefghij0123",
	"token prefix #xox":            "bot is xox" + "b-1234567890-abcdefghij",
	"token prefix #aws":            "id AK" + "IAABCDEFGHIJKLMNOP",
}

var secretControls = []string{
	"the token lives in the vault",
	"The deploy script reads API_KEY from the vault entry named site-login.",
	"password: stored in the vault under the name db-main",
	"A bearer token is sent in the Authorization header.",
	"design tok" + "ens: tv-color-primary-600",
	"Design tok" + "ens: --tv-color-primary",
	"tok" + "enizer: sentencepiece_bpe_v2",
	"tok" + "en: vault/projects/x/ssh-deploy",
	"pas" + "sword: projects/site/wp-admin-login",
	"sec" + "rets: projects/site/deploy-key-main",
	"TOK" + "EN_URL=example.com/oauth/v2/token",
	"PAS" + "SWORD_FILE=/run/secrets/db_password",
	"pri" + "vate_key_path: /etc/ssl/private/site.key",
	"API" + "_KEY=process.env.OPENAI_API_KEY",
	"SEC" + "RET_KEY_BASE: config/credentials.yml.enc",
	"api" + "_key: YOUR_API_KEY_GOES_HERE",
	"the spinner class is sk-loading-spinner-container-wide",
	"set API_KEY in .env",
	"postgres://user:${DB_PASSWORD}@db.example.invalid/main",
}

func TestSecretShapesRefusedOnLearnAndFactAdd(t *testing.T) {
	for rule, text := range secretSamples {
		t.Run(rule, func(t *testing.T) {
			st, _, repo := newStore(t)
			commits := gitOut(t, repo, "rev-list", "--all", "--count")
			for name, write := range map[string]func() error{
				"learn":    func() error { _, err := st.Learn(context.Background(), text, "p", "", ""); return err },
				"fact_add": func() error { _, err := st.AddFact(context.Background(), text, "p", "", 0); return err },
			} {
				err := write()
				if err == nil {
					t.Fatalf("%s stored a secret-shaped text", name)
				}
				if !strings.Contains(err.Error(), strings.SplitN(rule, " #", 2)[0]) || !strings.Contains(err.Error(), "vault") {
					t.Fatalf("%s error should name the rule %q and say what to do: %v", name, rule, err)
				}
				for _, frag := range []string{"abcd1234", "abcdefghij", "MIIEow", "1234567890", "ABCDEFGHIJ", "s3cretPass1", "wJalr", "Ab3dEf"} {
					if strings.Contains(err.Error(), frag) {
						t.Fatalf("%s error repeats the text: %v", name, err)
					}
				}
			}
			st.Commit.Flush()
			for _, d := range []string{"notes", "facts"} {
				if es, _ := os.ReadDir(filepath.Join(repo, d)); len(es) != 0 {
					t.Fatalf("%s holds %d entries after a refusal", d, len(es))
				}
			}
			if got := gitOut(t, repo, "rev-list", "--all", "--count"); got != commits {
				t.Fatalf("commit count changed from %q to %q", commits, got)
			}
			if out := gitOut(t, repo, "status", "--porcelain"); strings.TrimSpace(out) != "" {
				t.Fatalf("working tree not clean: %s", out)
			}
		})
	}
}

func TestSecretControlsAreStored(t *testing.T) {
	st, _, _ := newStore(t)
	for _, text := range secretControls {
		if _, err := st.Learn(context.Background(), text, "p", "", ""); err != nil {
			t.Errorf("learn refused %q: %v", text, err)
		}
		if _, err := st.AddFact(context.Background(), text, "p", "", 0); err != nil {
			t.Errorf("fact_add refused %q: %v", text, err)
		}
	}
}

// The credential shape is the one the commit hook uses; this fails if the hook's key words change and this copy does not.
func TestSecretKeyWordsMatchCommitHook(t *testing.T) {
	hook, err := os.ReadFile("../../scripts/hooks/pre-commit")
	if err != nil {
		t.Skip("source checkout not present")
	}
	if !strings.Contains(string(hook), credentialWords) {
		t.Fatalf("scripts/hooks/pre-commit no longer holds the key words %q", credentialWords)
	}
}

// Kind and category are written to the file's frontmatter, so they are checked like the text.
func TestSecretInKindAndCategoryRefused(t *testing.T) {
	st, _, repo := newStore(t)
	secret := secretSamples["credential assignment"]
	if _, err := st.Learn(context.Background(), "A plain note about the site layout.", "p", secret, ""); err == nil {
		t.Fatal("learn stored a secret in kind")
	}
	if _, err := st.AddFact(context.Background(), "A plain fact about the site layout.", "p", secret, 0); err == nil {
		t.Fatal("fact_add stored a secret in category")
	}
	st.Commit.Flush()
	for _, d := range []string{"notes", "facts"} {
		if es, _ := os.ReadDir(filepath.Join(repo, d)); len(es) != 0 {
			t.Fatalf("%s holds %d entries after a refusal", d, len(es))
		}
	}
}

// A refusal leaves a trace with the rule name only.
func TestSecretRefusalIsAnEventWithoutText(t *testing.T) {
	st, _, _ := newStore(t)
	text := secretSamples["bearer token"]
	st.Learn(context.Background(), text, "p", "", "")
	var kind, detail string
	if err := st.ev.QueryRow(`SELECT kind, detail FROM events WHERE kind='learn_refused'`).Scan(&kind, &detail); err != nil {
		t.Fatalf("no refusal event: %v", err)
	}
	if detail != "bearer token" || strings.Contains(detail, "abcdefghij") {
		t.Fatalf("event detail %q should be the rule name only", detail)
	}
}
