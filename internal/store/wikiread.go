package store

import (
	"fmt"
	"github.com/tomRumi/harness-memory/internal/index"
	"os"
	"path/filepath"
	"strings"
)

// WikiRead returns a wiki page, given its path under wiki/ (project/Title.md, with or without the wiki/ prefix) or
// its title (Title or project/Title, any case, or an alias). A title is resolved the way a [[link]] is. The page is
// followed by a footer naming the pages it links to, the pages that link to it and the pages closest in meaning.
func (s *Store) WikiRead(path string) (string, error) { return s.WikiReadFor(path, nil) }

// WikiReadFor is WikiRead for a caller that may see only some projects: allow says whether a project may be seen. A
// page of another project answers like a page that does not exist, and the link lines name only allowed pages. A nil
// allow sees everything.
func (s *Store) WikiReadFor(path string, allow func(project string) bool) (string, error) {
	in := filepath.ToSlash(strings.TrimSpace(path))
	in = strings.TrimPrefix(in, "wiki/")
	if in == "" || strings.HasPrefix(in, "/") {
		return "", fmt.Errorf("give a path under wiki/ (project/Title.md) or a page title")
	}
	for _, seg := range strings.Split(in, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path leaves wiki/")
		}
	}
	if proj, _, _ := strings.Cut(in, "/"); strings.HasSuffix(in, ".md") && (allow == nil || allow(proj)) {
		text, err := s.readWikiFile(in)
		if err == nil {
			return text + s.linksFooter("wiki/"+strings.TrimSuffix(in, ".md"), allow), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	name := strings.TrimSuffix(in, ".md")
	id, matches, err := s.DB.ResolveTitle(name)
	if err != nil {
		return "", err
	}
	if allow != nil { // pages of other projects do not exist for this caller, and do not make a title ambiguous
		var seen []string
		for _, m := range append([]string{id}, matches...) {
			if m != "" && allow(pageProject(m)) {
				seen = append(seen, m)
			}
		}
		id, matches = "", seen
		if len(seen) == 1 {
			id, matches = seen[0], nil
		}
	}
	switch {
	case id != "":
		text, err := s.readWikiFile(strings.TrimPrefix(id, "wiki/") + ".md")
		if err != nil {
			return "", fmt.Errorf("no wiki entry %q", path)
		}
		return text + s.linksFooter(id, allow), nil
	case len(matches) > 1:
		return "", fmt.Errorf("several pages answer to %q: %s; give the project as project/Title", name, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("no wiki entry %q", path)
}

// readWikiFile reads wiki/<rel>, refusing a symlink that leads out of wiki/ or out of the page's own project folder. A missing file is an os.IsNotExist error.
func (s *Store) readWikiFile(rel string) (string, error) {
	root := filepath.Join(s.Repo, "wiki")
	full := filepath.Join(root, filepath.FromSlash(rel))
	if r, err := filepath.EvalSymlinks(root); err == nil {
		if f, err := filepath.EvalSymlinks(full); err == nil {
			rl, err := filepath.Rel(r, f)
			if err != nil || strings.HasPrefix(rl, "..") {
				return "", fmt.Errorf("path leaves wiki/")
			}
			if proj := pageProject("wiki/" + rel); proj != "" && !strings.HasPrefix(rl, proj+string(filepath.Separator)) {
				return "", os.ErrNotExist // a link into another project's folder: the page does not exist for this caller
			}
		}
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// pageProject is the project of a page id (wiki/<project>/<Title>); a page directly under wiki/ has none.
func pageProject(id string) string {
	proj, rest, found := strings.Cut(strings.TrimPrefix(id, "wiki/"), "/")
	if !found || rest == "" {
		return ""
	}
	return proj
}

// linkName is how a page is named in a footer: its file name, with the project in front when it is not in project.
func linkName(id, project string) string {
	rest := strings.TrimPrefix(id, "wiki/")
	proj, name, _ := strings.Cut(rest, "/")
	if proj == project {
		return filepath.Base(name)
	}
	return proj + "/" + filepath.Base(name)
}

// linksFooter returns the footer of wiki_read: "Links to: [[A]], [[B]]" and "Linked from: [[C]]", resolved links
// only, a line left out when it would be empty.
func (s *Store) linksFooter(id string, allow func(string) bool) string {
	proj, _, _ := strings.Cut(strings.TrimPrefix(id, "wiki/"), "/")
	var b strings.Builder
	for _, l := range []struct {
		label string
		get   func(string) ([]string, error)
	}{{"Links to", s.DB.OutLinks}, {"Linked from", s.DB.Backlinks}} {
		all, err := l.get(id)
		var ids []string
		for _, x := range all {
			if allow == nil || allow(pageProject(x)) {
				ids = append(ids, x)
			}
		}
		if err != nil || len(ids) == 0 {
			continue
		}
		names := make([]string, len(ids))
		for i, x := range ids {
			names[i] = "[[" + linkName(x, proj) + "]]"
		}
		b.WriteString(l.label + ": " + strings.Join(names, ", ") + "\n")
	}
	if r := s.relatedLine(id, proj, allow); r != "" {
		b.WriteString(r)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n---\n" + b.String()
}

// relatedLine is "Related: [[A]] (0.81), [[B]] (0.77)": the pages closest in meaning, at most index.RelatedShown with a
// cosine of at least m.RelatedMin(). It is empty when there are no vectors for the page or none that close. The
// embedder is never called: vectors already stored are used.
func (s *Store) relatedLine(id, proj string, allow func(string) bool) string {
	if index.IsSpecialPage(id) {
		return ""
	}
	var m *index.Matrix
	if s.Matrix != nil {
		m = s.Matrix()
	} else if s.Emb != nil {
		m, _ = s.DB.LoadMatrix(s.Emb.ID())
	}
	k := index.RelatedShown
	if allow != nil {
		k = -1 // cut after the filter, so pages of other projects do not take a place
	}
	rel, ok := m.RelatedPages(id, k)
	if !ok {
		return ""
	}
	var names []string
	for _, r := range rel {
		if allow != nil && (!allow(pageProject(r.NodeID)) || len(names) == index.RelatedShown) {
			continue
		}
		if r.Score >= m.RelatedMin() {
			names = append(names, fmt.Sprintf("[[%s]] (%.2f)", linkName(r.NodeID, proj), r.Score))
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "Related: " + strings.Join(names, ", ") + "\n"
}
