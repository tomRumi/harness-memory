// Package access holds the token files of `hms serve` and checks a bearer token against them. A token file is
// <dir>/<project>.json and lists the sha256 of each token allowed to use that project, with its rights there. The files
// are read again on every request, so removing one revokes its tokens at once. Any fault in the directory or in a file
// is an error that names it; the caller then refuses the request. There is no fallback to open mode.
package access

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/tomRumi/harness-memory/internal/wiki"
)

// Grant is what a token may do in one project. Verify lets it call verify; LearnStatus "unverified" makes it staged.
type Grant struct {
	Write       bool   `json:"write"`
	Verify      bool   `json:"verify"`
	LearnStatus string `json:"learn_status"` // "" or "unverified"
	Name        string `json:"name"`
}

type file struct {
	Version int              `json:"version"`
	Project string           `json:"project"`
	Tokens  map[string]Grant `json:"tokens"`
}

var digestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Digest is the lower-case hex sha256 of a token, the form the files hold.
func Digest(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Load reads every token file in dir and returns, for each token digest, its grant in each project. It fails on the
// first fault, and the error names the directory or file and what is wrong, never a digest or a token.
func Load(dir string) (map[string]map[string]Grant, error) {
	if err := checkMode(dir, true); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("token directory %s: cannot be read: %v", dir, err)
	}
	out := map[string]map[string]Grant{}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue // a dot name is a writer's temporary file
		}
		path := filepath.Join(dir, name)
		if err := checkMode(path, false); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("token file %s: cannot be read: %v", path, err)
		}
		f, err := parse(b, strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, fmt.Errorf("token file %s: %v", path, err)
		}
		for d, g := range f.Tokens {
			if out[d] == nil {
				out[d] = map[string]Grant{}
			}
			out[d][f.Project] = g
		}
	}
	return out, nil
}

func checkMode(path string, dir bool) error {
	what := "token file"
	if dir {
		what = "token directory"
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %v; create it, or unset HMS_TOKENS_DIR to run without tokens", what, path, err)
	}
	if dir && !fi.IsDir() {
		return fmt.Errorf("token directory %s: is not a directory", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s %s: writable by group or others (mode %04o); run chmod go-w on it", what, path, fi.Mode().Perm())
	}
	return nil
}

func parse(b []byte, base string) (*file, error) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("not valid JSON for a token file: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("not valid JSON for a token file: text after the object")
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("version is %d; only version 1 is known", f.Version)
	}
	if p, err := wiki.Project(f.Project); err != nil || p != f.Project {
		return nil, fmt.Errorf("project %q is not a valid project name", f.Project)
	}
	if f.Project != base {
		return nil, fmt.Errorf("project %q does not match the file name; the file must be %s.json", f.Project, f.Project)
	}
	for d, g := range f.Tokens {
		if !digestRe.MatchString(d) {
			return nil, fmt.Errorf("a token key is not 64 lower-case hex characters (the sha256 of the token)")
		}
		if g.LearnStatus != "" && g.LearnStatus != "unverified" {
			return nil, fmt.Errorf("learn_status must be \"\" or \"unverified\", not %q", g.LearnStatus)
		}
	}
	return &f, nil
}

// Rights are the grants of the token of one call, by project.
type Rights struct {
	Digest string
	Grants map[string]Grant
}

// Projects lists the granted projects, sorted.
func (r *Rights) Projects() []string {
	var ps []string
	for p := range r.Grants {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// Allows says whether the token is granted the project.
func (r *Rights) Allows(project string) bool { _, ok := r.Grants[project]; return ok }

// Resolve picks the project of a call. A named project must be granted; an empty one means the token's only project.
func (r *Rights) Resolve(project string) (string, error) {
	if strings.TrimSpace(project) != "" {
		p, err := wiki.Project(project)
		if err != nil {
			return "", err
		}
		if !r.Allows(p) {
			return "", fmt.Errorf("this token may not use project %s", p)
		}
		return p, nil
	}
	ps := r.Projects()
	if len(ps) == 1 {
		return ps[0], nil
	}
	return "", fmt.Errorf("give a project; this token may name: %s", strings.Join(ps, ", "))
}

// Of returns the rights the verifier put in the token info of a call, or nil when there are none.
func Of(ti *auth.TokenInfo) *Rights {
	if ti == nil {
		return nil
	}
	r, _ := ti.Extra["rights"].(*Rights)
	return r
}

// Verifier is the auth.TokenVerifier of token mode. An unknown token is auth.ErrInvalidToken (a 401 without detail);
// a fault in the files is another error, which the middleware answers with a 500 that names it.
func Verifier(dir string) auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		all, err := Load(dir)
		if err != nil {
			return nil, err
		}
		d := Digest(token)
		g, ok := all[d]
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: d, Extra: map[string]any{"rights": &Rights{Digest: d, Grants: g}}}, nil
	}
}
