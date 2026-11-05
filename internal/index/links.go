package index

import (
	"database/sql"
	"sort"
	"strings"

	"github.com/tomRumi/harness-memory-service/internal/wiki"
)

// Link states kept on a body link edge. The raw link text stays in object_ref; resolved holds the node id of the
// page it names when the state is linkOK.
const (
	linkOK        = "ok"
	linkBroken    = "broken"
	linkAmbiguous = "ambiguous"
)

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	Exec(query string, args ...any) (sql.Result, error)
}

type target struct{ id, project string }

// resolver finds the wiki page a link names, the way Obsidian does: by file name or alias, ignoring case.
type resolver struct {
	byKey    map[string][]target // lower-case name -> pages that answer to it
	projects map[string]string   // lower-case project -> project
}

func loadResolver(q queryer) (*resolver, error) {
	rows, err := q.Query(`SELECT id, project, stem, aliases FROM nodes WHERE kind='wiki' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	r := &resolver{byKey: map[string][]target{}, projects: map[string]string{}}
	for rows.Next() {
		var id, project, stem, aliases string
		if err := rows.Scan(&id, &project, &stem, &aliases); err != nil {
			return nil, err
		}
		r.projects[strings.ToLower(project)] = project
		keys := map[string]bool{strings.ToLower(stem): true}
		keys[strings.ToLower(strings.TrimPrefix(id, "wiki/"+project+"/"))] = true // a path inside the project: [[folder/Page]]
		for _, a := range strings.Split(aliases, "\n") {
			if a = strings.TrimSpace(a); a != "" {
				keys[strings.ToLower(a)] = true
			}
		}
		for k := range keys {
			r.byKey[k] = append(r.byKey[k], target{id, project})
		}
	}
	return r, rows.Err()
}

// resolve returns the state of a link written in a node of project src. For linkOK, id is the page; for
// linkAmbiguous, matches lists the pages that answer to the name, sorted.
func (r *resolver) resolve(src, raw string) (status, id string, matches []string) {
	name := strings.TrimSpace(raw)
	if len(name) > 3 && strings.EqualFold(name[len(name)-3:], ".md") {
		name = name[:len(name)-3]
	}
	pick := func(ts []target) (string, string, []string, bool) {
		var ids []string
		seen := map[string]bool{}
		for _, t := range ts {
			if !seen[t.id] {
				seen[t.id] = true
				ids = append(ids, t.id)
			}
		}
		sort.Strings(ids)
		switch len(ids) {
		case 0:
			return "", "", nil, false
		case 1:
			return linkOK, ids[0], nil, true
		}
		return linkAmbiguous, "", ids, true
	}
	if i := strings.Index(name, "/"); i > 0 { // [[project/Target]]
		if proj, ok := r.projects[strings.ToLower(name[:i])]; ok {
			var in []target
			for _, t := range r.byKey[strings.ToLower(name[i+1:])] {
				if t.project == proj {
					in = append(in, t)
				}
			}
			if s, id, m, ok := pick(in); ok {
				return s, id, m
			}
		}
	}
	all := r.byKey[strings.ToLower(name)]
	var same, other []target
	for _, t := range all {
		if t.project == src {
			same = append(same, t)
		} else {
			other = append(other, t)
		}
	}
	if s, id, m, ok := pick(same); ok {
		return s, id, m
	}
	if s, id, m, ok := pick(other); ok {
		return s, id, m
	}
	return linkBroken, "", nil
}

// resolveLinks recomputes the state of every body link from the pages that exist now. It runs after every change to
// the node set, so adding, renaming or deleting a page fixes or breaks the links to it. It writes only what differs.
func resolveLinks(q queryer) error {
	r, err := loadResolver(q)
	if err != nil {
		return err
	}
	type row struct{ subject, ref, resolved, status string }
	rows, err := q.Query(`SELECT e.subject_id, e.object_ref, n.project, e.resolved, e.status FROM edges e
		JOIN nodes n ON n.id = e.subject_id WHERE e.relation='links_to' AND e.source='body'`)
	if err != nil {
		return err
	}
	var todo []row
	for rows.Next() {
		var s, ref, proj, res, st string
		if err := rows.Scan(&s, &ref, &proj, &res, &st); err != nil {
			rows.Close()
			return err
		}
		nst, nid, _ := r.resolve(proj, ref)
		if nst != st || nid != res {
			todo = append(todo, row{s, ref, nid, nst})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, u := range todo {
		if _, err := q.Exec(`UPDATE edges SET resolved=?, status=? WHERE subject_id=? AND relation='links_to' AND source='body' AND object_ref=?`,
			u.resolved, u.status, u.subject, u.ref); err != nil {
			return err
		}
	}
	return nil
}

// ResolveTitle finds the wiki page a title names, as a link written outside any project would: [[Title]] or
// [[project/Title]], case-insensitive, by file name or alias. A name that is not unique across projects is not
// resolved: matches then lists the candidate node ids. Neither id nor matches is set when no page answers.
func (d *DB) ResolveTitle(name string) (id string, matches []string, err error) {
	r, err := loadResolver(d)
	if err != nil {
		return "", nil, err
	}
	st, id, m := r.resolve("", name)
	if st == linkOK {
		return id, nil, nil
	}
	return "", m, nil
}

// LinkResolver returns a function that says whether a link written in a page of project leads to exactly one
// existing page (an ambiguous link does not). It reads the pages of the index once.
func (d *DB) LinkResolver(project string) (func(link string) bool, error) {
	r, err := loadResolver(d)
	if err != nil {
		return nil, err
	}
	return func(link string) bool { st, _, _ := r.resolve(project, link); return st == linkOK }, nil
}

// LinkProblem is one link of a wiki page that does not lead to exactly one page.
type LinkProblem struct {
	Project string
	Page    string   // path of the page that holds the link, for example wiki/p/Title.md
	Link    string   // the link text as written (without |text or #heading)
	Status  string   // "broken" or "ambiguous"
	Matches []string // for an ambiguous link: the pages it could mean
}

// LinkProblems lists the broken and ambiguous links of wiki pages, in order of project, page and link text.
// project "" means every project.
func (d *DB) LinkProblems(project string) ([]LinkProblem, error) {
	q := `SELECT n.project, n.path, e.object_ref, e.status FROM edges e JOIN nodes n ON n.id = e.subject_id
		WHERE n.kind='wiki' AND e.relation='links_to' AND e.source='body' AND e.status IN ('broken','ambiguous')`
	var args []any
	if project != "" {
		q += ` AND n.project=?`
		args = append(args, project)
	}
	rows, err := d.Query(q+` ORDER BY n.project, n.path, e.object_ref`, args...)
	if err != nil {
		return nil, err
	}
	var out []LinkProblem
	for rows.Next() {
		var p LinkProblem
		if err := rows.Scan(&p.Project, &p.Page, &p.Link, &p.Status); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	r, err := loadResolver(d)
	if err != nil {
		return nil, err
	}
	for i, p := range out {
		if p.Status == linkAmbiguous {
			_, _, out[i].Matches = r.resolve(p.Project, p.Link)
			for j := range out[i].Matches {
				out[i].Matches[j] += ".md"
			}
		}
	}
	return out, nil
}

// LinkCounts returns the number of broken and of ambiguous links in wiki pages.
func (d *DB) LinkCounts() (broken, ambiguous int, err error) {
	rows, err := d.Query(`SELECT e.status, count(*) FROM edges e JOIN nodes n ON n.id = e.subject_id
		WHERE n.kind='wiki' AND e.relation='links_to' AND e.source='body' AND e.status IN ('broken','ambiguous') GROUP BY e.status`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return 0, 0, err
		}
		if s == linkBroken {
			broken = n
		} else {
			ambiguous = n
		}
	}
	return broken, ambiguous, rows.Err()
}

// Backlinks returns the ids of the wiki pages that link to the page id, sorted. A page's link to itself is left out.
func (d *DB) Backlinks(id string) ([]string, error) {
	return d.strings(`SELECT DISTINCT e.subject_id FROM edges e JOIN nodes n ON n.id = e.subject_id
		WHERE n.kind='wiki' AND e.relation='links_to' AND e.source='body' AND e.resolved=? AND e.subject_id<>? ORDER BY e.subject_id`, id, id)
}

// OutLinks returns the ids of the pages the page id links to (resolved links only), sorted, without itself.
func (d *DB) OutLinks(id string) ([]string, error) {
	return d.strings(`SELECT DISTINCT resolved FROM edges WHERE subject_id=? AND relation='links_to' AND source='body'
		AND status='ok' AND resolved<>subject_id ORDER BY resolved`, id)
}

func (d *DB) strings(q string, args ...any) ([]string, error) {
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Projects returns the projects that have wiki pages, sorted (pages directly under wiki/ belong to no project).
func (d *DB) Projects() ([]string, error) {
	return d.strings(`SELECT DISTINCT project FROM nodes WHERE kind='wiki' AND project<>'' ORDER BY project`)
}

// BuildIndexPage returns the markdown of wiki/<project>/Index.md: one line per page of the project, by title,
// without the index page itself.
func (d *DB) BuildIndexPage(project string) (string, error) {
	rows, err := d.Query(`SELECT id, stem, summary FROM nodes WHERE kind='wiki' AND project=?`, project)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var es []wiki.Entry
	self := "wiki/" + project + "/"
	for rows.Next() {
		var id, stem, summary string
		if err := rows.Scan(&id, &stem, &summary); err != nil {
			return "", err
		}
		if strings.EqualFold(id, self+wiki.IndexTitle) { // the index never lists itself
			continue
		}
		es = append(es, wiki.Entry{Name: stem, Summary: summary})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return wiki.IndexPage(es), nil
}
