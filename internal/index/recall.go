package index

import (
	"fmt"
	"regexp"
	"strings"
)

// Hit is one chunk returned by recall.
type Hit struct {
	NodeID, Kind, Project, Path, Section, Text string
	Score                                      float64
}

// Filter narrows a recall. Empty fields mean "any". Only active nodes are ever returned.
type Filter struct {
	Project string
	Kinds   []string
	Limit   int
}

var word = regexp.MustCompile(`[A-Za-z0-9_]+`)

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be by can do does for from has have how in is it its of on or that the this to was what when where which who why will with not no than then there their they you your into over under about each per via also any all out up off`) {
		stop[w] = true
	}
}

// MatchQuery turns free text into an FTS5 query: each word that is not a stop word becomes a quoted term and
// the terms are OR-ed, so the bm25 weight of rare terms decides the order. It returns "" when nothing is left.
func MatchQuery(q string) string {
	seen := map[string]bool{}
	var terms []string
	for _, w := range word.FindAllString(strings.ToLower(q), -1) {
		if len(w) < 2 || stop[w] || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"`)
	}
	return strings.Join(terms, " OR ")
}

// Keyword searches the keyword index. The best match comes first.
func (d *DB) Keyword(query string, f Filter) ([]Hit, error) {
	mq := MatchQuery(query)
	if mq == "" {
		return nil, nil
	}
	if f.Limit <= 0 {
		f.Limit = 5
	}
	sqlq := `SELECT n.id, n.kind, n.project, n.path, c.section, c.text, bm25(chunks_fts) AS score
	  FROM chunks_fts JOIN chunks c ON c.id = chunks_fts.rowid JOIN nodes n ON n.id = c.node_id
	  WHERE chunks_fts MATCH ? AND n.status = 'active'`
	args := []any{mq}
	if f.Project != "" {
		sqlq += ` AND n.project = ?`
		args = append(args, f.Project)
	}
	if len(f.Kinds) > 0 {
		sqlq += ` AND n.kind IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",") + `)`
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	sqlq += ` ORDER BY score, n.id, c.seq LIMIT ?`
	args = append(args, f.Limit)
	rows, err := d.Query(sqlq, args...)
	if err != nil {
		return nil, fmt.Errorf("keyword search: %w", err)
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.NodeID, &h.Kind, &h.Project, &h.Path, &h.Section, &h.Text, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Stats counts what the index holds, by kind and project.
func (d *DB) Stats() (map[string]int, error) {
	rows, err := d.Query(`SELECT kind || '/' || project, count(*) FROM nodes GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
