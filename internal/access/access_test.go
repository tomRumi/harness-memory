package access

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

const goodDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func dirWith(t *testing.T, files map[string]string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "tokens")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(d, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestLoadAndVerify(t *testing.T) {
	tok := "secret-token"
	d := dirWith(t, map[string]string{
		"a.json":      `{"version":1,"project":"a","tokens":{"` + Digest(tok) + `":{"write":true,"name":"x"}}}`,
		"global.json": `{"version":1,"project":"global","tokens":{"` + Digest(tok) + `":{}}}`,
		".b.json.tmp": `garbage`,
		".c.json":     `garbage`,
		"notes.txt":   `garbage`,
	})
	ti, err := Verifier(d)(nil, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := Of(ti)
	if ti.UserID != Digest(tok) || r == nil || strings.Join(r.Projects(), ",") != "a,global" || !r.Grants["a"].Write || r.Grants["global"].Write {
		t.Fatalf("%+v %+v", ti, r)
	}
	if _, err := Verifier(d)(nil, "other", nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := r.Resolve(""); err == nil || !strings.Contains(err.Error(), "a, global") {
		t.Fatalf("several projects, none named: %v", err)
	}
	if _, err := r.Resolve("b"); err == nil || !strings.Contains(err.Error(), "may not use project b") {
		t.Fatalf("%v", err)
	}
	os.Remove(filepath.Join(d, "global.json"))
	ti, _ = Verifier(d)(nil, tok, nil)
	if p, err := Of(ti).Resolve(""); err != nil || p != "a" {
		t.Fatalf("one project left: %q %v", p, err)
	}
	os.Remove(filepath.Join(d, "a.json"))
	if _, err := Verifier(d)(nil, tok, nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("removed file must revoke: %v", err)
	}
}

func TestFaultsAreNamedAndNeverShowADigest(t *testing.T) {
	ok := `"tokens":{"` + goodDigest + `":{}}`
	bad := map[string]string{
		"not json":        `{`,
		"unknown key":     `{"version":1,"project":"a","extra":1,` + ok + `}`,
		"unknown right":   `{"version":1,"project":"a","tokens":{"` + goodDigest + `":{"admin":true}}}`,
		"version":         `{"version":2,"project":"a",` + ok + `}`,
		"project invalid": `{"version":1,"project":"A b",` + ok + `}`,
		"other project":   `{"version":1,"project":"b",` + ok + `}`,
		"bad digest":      `{"version":1,"project":"a","tokens":{"ABC":{}}}`,
		"learn_status":    `{"version":1,"project":"a","tokens":{"` + goodDigest + `":{"learn_status":"active"}}}`,
	}
	for name, content := range bad {
		d := dirWith(t, map[string]string{"a.json": content})
		_, err := Load(d)
		if err == nil || !strings.Contains(err.Error(), filepath.Join(d, "a.json")) || strings.Contains(err.Error(), goodDigest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// file group- or world-writable
	d := dirWith(t, map[string]string{"a.json": `{"version":1,"project":"a",` + ok + `}`})
	os.Chmod(filepath.Join(d, "a.json"), 0o666)
	if _, err := Load(d); err == nil || !strings.Contains(err.Error(), "a.json") || !strings.Contains(err.Error(), "writable") {
		t.Errorf("writable file: %v", err)
	}
	// directory writable, missing, not a directory
	os.Chmod(filepath.Join(d, "a.json"), 0o644)
	os.Chmod(d, 0o777)
	if _, err := Load(d); err == nil || !strings.Contains(err.Error(), d) || !strings.Contains(err.Error(), "writable") {
		t.Errorf("writable dir: %v", err)
	}
	if _, err := Load(filepath.Join(d, "none")); err == nil || !strings.Contains(err.Error(), "none") {
		t.Errorf("missing dir: %v", err)
	}
	if _, err := Load(filepath.Join(d, "a.json")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file as dir: %v", err)
	}
}

// A token that may verify must not be staged: it would check its own notes.
func TestGrantStagedAndVerifyIsRefused(t *testing.T) {
	d := dirWith(t, map[string]string{
		"a.json": `{"version":1,"project":"a","tokens":{"` + goodDigest + `":{"write":true,"verify":true,"learn_status":"unverified"}}}`,
	})
	_, err := Load(d)
	if err == nil || !strings.Contains(err.Error(), "a.json") {
		t.Errorf("want an error naming a.json, got %v", err)
	}
}
