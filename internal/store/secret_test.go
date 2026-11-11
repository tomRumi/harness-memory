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
	"credential assignment": "the site login is set with API" + "_KEY=" + "abcd1234efgh5678ijkl",
	"private key block":     "key follows: -----BEGIN RSA PRIVATE" + " KEY----- MIIEow",
	"bearer token":          "send header Authorization: Bearer " + "abcdefghij0123456789ABCDEF",
	"token prefix":          "use sk" + "-abcdefghij0123456789abcd for the service",
	"token prefix #gh":      "pushed with gh" + "p_abcdefghij0123456789abcdefghij0123",
	"token prefix #xox":     "bot is xox" + "b-1234567890-abcdefghij",
	"token prefix #aws":     "id AK" + "IAABCDEFGHIJKLMNOP",
}

var secretControls = []string{
	"the token lives in the vault",
	"The deploy script reads API_KEY from the vault entry named site-login.",
	"password: stored in the vault under the name db-main",
	"A bearer token is sent in the Authorization header.",
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
				for _, frag := range []string{"abcd1234", "abcdefghij", "MIIEow", "1234567890", "ABCDEFGHIJ"} {
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
