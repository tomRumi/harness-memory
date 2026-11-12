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
	"credential assignment":         "the site login is set with API" + "_KEY=" + "abcd1234efgh5678ijkl",
	"token prefix #gl":              "ci uses glp" + "at-abcdefghij0123456789",
	"token prefix #ghpat":           "github_p" + "at_abcdefghij0123456789ABCD",
	"token prefix #live":            "billing sk" + "_live_abcdefghij0123456789",
	"token prefix #aiza":            "maps AI" + "zaSyA-abcdefghij0123456789abcdefghijk",
	"credential assignment #space":  "the api k" + "ey: abcd1234efgh5678ijkl",
	"credential assignment #url":    "call https://example.invalid/cb?tok" + "en=Ab3dEf6hIj9lMn2pQr5tUv",
	"credential assignment #define": "define('DB_PASS" + "WORD', 'Xk29fjq0Lmz81PqR');",
	"credential assignment #json":   "{\"pass" + "word\": \"Xk29fjq0Lmz81PqR\"}",
	"credential assignment #arrow":  "'pass" + "word' => 'Xk29fjq0Lmz81PqR',",
	"credential assignment #b64":    "AWS_SECRET_ACCESS" + "_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYzzzzzzzzzz",
	"web token":                     "session eyJ" + "hbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkw.abcdefghijKLMNOP",
	"url credentials":               "db at postgres://app:" + "s3cretPass1@db.example.invalid/main",
	"private key block":             "key follows: -----BEGIN RSA PRIVATE" + " KEY----- MIIEow",
	"bearer token":                  "send header Authorization: Bearer " + "abcdefghij0123456789ABCDEF",
	"token prefix":                  "use sk" + "-abcdefghij0123456789abcd for the service",
	"token prefix #gh":              "pushed with gh" + "p_abcdefghij0123456789abcdefghij0123",
	"token prefix #xox":             "bot is xox" + "b-1234567890-abcdefghij",
	"token prefix #aws":             "id AK" + "IAABCDEFGHIJKLMNOP",

	"credential assignment #symbolpw":  "define('DB_PASS" + "WORD', 'p@ssW0rd!Xk29fjq0Lmz');",
	"credential assignment #pw12":      "define('DB_PASS" + "WORD', 'p@ssW0rd!x9z');",
	"credential assignment #smtp":      "define('WPMS_SMTP_PASS', 'Xk29fjq0Lmz81PqR');",
	"credential assignment #smtpenv":   "WPMS_SMTP_PASS=Xk29fjq0Lmz81PqR",
	"credential assignment #cache":     "define('WP_CACHE_KEY_SALT', 'q8" + "Wm!zL;t2^Rb#Vn5&Hc@Xd3$Ke9');",
	"credential assignment #plainsalt": "define('NONCE_SALT', '" + "qWmzLtRbVnHcXdKeJfGyPsUaTiOo" + "ZxCvBnMlKjHg');",
	"credential assignment #wpcli":     "run wp config set DB_PASS" + "WORD 'p@ssW0rd!Xk29fjq0Lmz' --type=constant",
	"credential assignment #wpclibare": "wp config set AUTH_KEY " + "k;Q2|8xvN!b%Zr1^Lq&3Tz]Yd+0Hf~Gw4Cm>Js7Pe<Ua",
	"credential assignment #wpclisalt": "wp config set --type=constant LOGGED_IN_SALT '" + "k;Q2|8xvN!b%Zr1^Lq&3Tz]Yd+0Hf~Gw4Cm>Js7Pe<Ua'",
	"credential assignment #yamlsalt":  "AUTH_KEY: \"" + "k;Q2|8xvN!b%Zr1^Lq&3Tz]Yd+0Hf~Gw4Cm>Js7Pe<Ua\"",

	"credential assignment #dollar":      "define('AUTH_KEY', '" + "$Q2|8xvN!b%Zr1^Lq&3Tz]Yd+0Hf~Gw4Cm>Js7Pe<Ua" + "');",
	"credential assignment #angle":       "define('NONCE_SALT', '" + "<Q2|8xvN!b%Zr1^Lq&3Tz]Yd+0Hf~Gw4Cm>Js7Pe<Ua" + "');",
	"credential assignment #dollarpw":    "define('DB_PASS" + "WORD', '$" + "p@ssW0rd!Xk29fjq0Lmz');",
	"credential assignment #percentsalt": "define('LOGGED_IN_SALT', '%" + "Q2|8xvN!b%Zr1^Lq&3Tz]Yd+0Hf~Gw4Cm>Js7Pe<Ua" + "');",
	"credential assignment #parensalt":   "define('SECURE_AUTH_KEY', '(" + "Q2|8xvN!b%Zr1^Lq&3Tz]Yd+0Hf~Gw4Cm>Js7Pe<Ua" + "');",
	"credential assignment #dbpass":      "wp config create --dbname=site --db" + "pass='p@ssW0rd!Xk29fjq0Lmz' --dbuser=app",
	"credential assignment #dbpassbare":  "wp config create --db" + "pass=Xk29fjq0Lmz81PqR",
	"credential assignment #wppath":      "wp --path=/var/www/site config set DB_PASS" + "WORD 'p@ssW0rd!Xk29fjq0Lmz'",
}

// The eight WordPress key and salt lines: random by nature, so any long value counts, with symbols or without a digit.
func init() {
	for _, n := range []string{"AUTH_KEY", "SECURE_AUTH_KEY", "LOGGED_IN_KEY", "NONCE_KEY", "AUTH_SALT", "SECURE_AUTH_SALT", "LOGGED_IN_SALT", "NONCE_SALT"} {
		secretSamples["credential assignment #salt-"+n] = "define('" + n + "',  '" + "k;Q2|8xvN!b%Zr1^Lq&3Tz]Yd+0Hf~Gw4Cm>Js7Pe<Ua" + "');"
		secretSamples["credential assignment #saltplain-"+n] = "define( \"" + n + "\", \"" + "qWmzLtRbVnHcXdKeJfGyPsUaTiOoZxCvBnMlKjHgFdSaPoIuYtRe" + "\" );"
	}
}

var secretControls = []string{
	"the token lives in the vault",
	"The deploy script reads API_KEY from the vault entry named site-login.",
	"password: stored in the vault under the name db-main",
	"A bearer token is sent in the Authorization header.",
	"design tok" + "ens: ui-color-primary-600",
	"Design tok" + "ens: --ui-color-primary",
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
	"define('WP_DEBUG', false);",
	"define('DB_NAME', 'wordpress');",
	"define('DB_PASS" + "WORD', 'wordpress');",
	"{\"pass" + "word\": \"projects/site/wp-admin-login\"}",
	"postgres://user:${DB_PASSWORD}@db.example.invalid/main",
	"define('AUTH_KEY',         'put your unique phrase here');",
	"define('SECURE_AUTH_SALT', 'put your unique phrase here');",
	"define('DB_PASS" + "WORD', getenv('DB_PASS" + "WORD_FILE_1'));",
	"define('WP_CACHE_KEY_SALT', getenv('CACHE_SALT_1'));",
	"define('WP_CACHE_KEY_SALT', 'site-cache');",
	"define('WPMS_SMTP_PASS', 'wordpress');",
	"define('DB_PASS" + "WORD', 'see the vault entry 2');",
	"The salts AUTH_KEY and NONCE_SALT are generated once per site and kept in the vault.",
	"wp config set DB_PASS" + "WORD <value>",
	"wp config set DB_PASS" + "WORD $DB_PASS" + "WORD_FILE_1",
	"wp config set WP_DEBUG true --raw",
	"wp config set DB_NAME wordpress_site_2026",
	"wp config set BYPASS_CACHE_ON_COOKIE wordpress_logged_in_2026",
	"wp config create --dbname=site --db" + "pass=$DB_PASS" + "WORD_1",
	"wp config create --dbname=site --dbuser=app --dbhost=127.0.0.1",
	"wp --path=/var/www/site config set WP_DEBUG true --raw",
	"define('WPMS_SMTP_PASS', '${SMTP_PASS_1}');",
	"define('AUTH_KEY', '<put your unique phrase here>');",
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
				for _, frag := range []string{"abcd1234", "abcdefghij", "MIIEow", "1234567890", "ABCDEFGHIJ", "s3cretPass1", "wJalr", "Ab3dEf", "Zr1^Lq", "qWmzLt", "Xk29fj", "p@ssW0", "Q2|8xv"} {
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
