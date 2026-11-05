package index

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/wiki"
)

// Page vectors are CENTRED: the mean of all page vectors is subtracted and the result is normalised again. Pages of one
// wiki share a style and a subject area, so raw cosines sit in a narrow high band (0.65 to 0.99 on the test wiki) and
// say little; after centring the shared part is gone and related pages score above 0, unrelated ones below. With fewer
// than CenterMinPages pages there is too little to estimate the shared part from and raw vectors are used.
//
// Frozen settings of the meaning-based page features. Like the retrieval settings they are constants: a change is
// a new commit with a new score from the pair evaluation (hms wiki eval-pairs), not a runtime option.
const (
	// RelatedFloor is the lowest cosine at which wiki_read lists a page as related.
	RelatedFloor = 0.0
	// RelatedFloorRaw is the same floor for a wiki too small to centre (raw cosines).
	RelatedFloorRaw = 0.70
	// CenterMinPages is the number of wiki pages from which page vectors are centred.
	CenterMinPages = 8
	// DuplicateThreshold is the cosine of two page vectors from which they are reported as near-duplicates.
	DuplicateThreshold = 0.80
	// RelatedShown is the number of related pages wiki_read lists.
	RelatedShown = 5
)

// PageScore is a wiki page with its cosine to the page it was compared with.
type PageScore struct {
	NodeID  string // wiki/<project>/<Title>
	Project string
	Title   string
	Score   float32
}

// PagePair is two near-duplicate pages with the cosine of their page vectors.
type PagePair struct {
	A, B  PageScore // the Score of each is left 0
	Score float32
}

// IsSpecialPage says whether the wiki node id is an Index or an Unsorted page: pages that are lists, not subjects.
func IsSpecialPage(id string) bool {
	return wiki.IsSpecial(pageTitle(id))
}

func pageTitle(id string) string { return id[strings.LastIndex(id, "/")+1:] }

type pageSet struct {
	ids     []string // sorted
	project map[string]string
	vec     map[string][]float32 // unit length, centred when mean is not nil
	mean    []float32            // mean of the raw page vectors, nil when there are fewer than CenterMinPages pages
}

// center puts a raw (unit) page vector into the same space as the page vectors of this set.
func (ps *pageSet) center(v []float32) []float32 {
	if ps.mean == nil || len(v) != len(ps.mean) {
		return v
	}
	d := make([]float64, len(v))
	for i := range v {
		d[i] = float64(v[i] - ps.mean[i])
	}
	if u := unit64(d); u != nil {
		return u
	}
	return v
}

// pages builds the page vectors once per Matrix: the L2-normalised mean of the vectors of each wiki page's chunks
// (only the chunks that have a vector). A page whose mean is the zero vector is left out.
func (m *Matrix) pages() *pageSet {
	m.pagesOnce.Do(func() {
		ps := &pageSet{project: map[string]string{}, vec: map[string][]float32{}}
		sums := map[string][]float64{}
		for i, id := range m.Node {
			if m.Kind[i] != "wiki" {
				continue
			}
			s := sums[id]
			if s == nil {
				s = make([]float64, m.Dim)
				sums[id] = s
				ps.project[id] = m.Project[i]
			}
			for j, x := range m.vecs[i*m.Dim : (i+1)*m.Dim] {
				s[j] += float64(x)
			}
		}
		for id, s := range sums {
			if v := unit64(s); v != nil {
				ps.vec[id] = v
				ps.ids = append(ps.ids, id)
			}
		}
		sort.Strings(ps.ids)
		if len(ps.ids) >= CenterMinPages {
			mean := make([]float64, m.Dim)
			for _, id := range ps.ids {
				for j, x := range ps.vec[id] {
					mean[j] += float64(x)
				}
			}
			ps.mean = make([]float32, m.Dim)
			for j := range mean {
				ps.mean[j] = float32(mean[j] / float64(len(ps.ids)))
			}
			for _, id := range ps.ids {
				ps.vec[id] = ps.center(ps.vec[id])
			}
		}
		m.pageSet = ps
	})
	return m.pageSet
}

func unit64(s []float64) []float32 {
	var n float64
	for _, x := range s {
		n += x * x
	}
	if n == 0 {
		return nil
	}
	n = math.Sqrt(n)
	v := make([]float32, len(s))
	for i, x := range s {
		v[i] = float32(x / n)
	}
	return v
}

func dot(a, b []float32) float32 {
	var s float32
	for i, x := range a {
		s += x * b[i]
	}
	return s
}

// PageVector returns the vector of the wiki page nodeID (a copy). ok is false when the page has no vectors yet.
func (m *Matrix) PageVector(nodeID string) ([]float32, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m.pages().vec[nodeID]
	return append([]float32(nil), v...), ok
}

// Cosine is the cosine of the page vectors of two wiki pages; ok is false when either has no vector.
func (m *Matrix) Cosine(a, b string) (float32, bool) {
	if m == nil {
		return 0, false
	}
	ps := m.pages()
	va, oka := ps.vec[a]
	vb, okb := ps.vec[b]
	if !oka || !okb {
		return 0, false
	}
	return dot(va, vb), true
}

// PageInfo is a wiki page as listed by WikiPages.
type PageInfo struct {
	NodeID, Project, Title, Summary string
}

// WikiPages lists the wiki pages of a project (every project when empty) without Index and Unsorted pages, by id.
func (d *DB) WikiPages(project string) ([]PageInfo, error) {
	rows, err := d.Query(`SELECT id, project, summary FROM nodes WHERE kind='wiki' AND (?='' OR project=?) ORDER BY id`, project, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PageInfo
	for rows.Next() {
		var p PageInfo
		if err := rows.Scan(&p.NodeID, &p.Project, &p.Summary); err != nil {
			return nil, err
		}
		if p.Title = pageTitle(p.NodeID); !IsSpecialPage(p.NodeID) {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// PageIDs lists the wiki pages that have a vector, sorted.
func (m *Matrix) PageIDs() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.pages().ids...)
}

func (ps *pageSet) score(id string, s float32) PageScore {
	return PageScore{NodeID: id, Project: ps.project[id], Title: pageTitle(id), Score: s}
}

// RelatedPages returns the k wiki pages whose vectors are closest to the page nodeID, best first, without the page
// itself and without Index and Unsorted pages. Pages of the same project come first only between equal scores.
// ok is false when the page has no vector.
func (m *Matrix) RelatedPages(nodeID string, k int) (out []PageScore, ok bool) {
	if m == nil {
		return nil, false
	}
	ps := m.pages()
	q, ok := ps.vec[nodeID]
	if !ok {
		return nil, false
	}
	for _, id := range ps.ids {
		if id == nodeID || IsSpecialPage(id) {
			continue
		}
		out = append(out, ps.score(id, dot(q, ps.vec[id])))
	}
	home := ps.project[nodeID]
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		if sa, sb := out[a].Project == home, out[b].Project == home; sa != sb {
			return sa
		}
		return out[a].NodeID < out[b].NodeID
	})
	if k >= 0 && len(out) > k {
		out = out[:k]
	}
	return out, true
}

// NearDuplicates returns each pair of wiki pages whose page vectors have a cosine >= threshold, best first, a pair
// once. With a project, only pairs of two pages of that project; Index and Unsorted pages are left out.
func (m *Matrix) NearDuplicates(project string, threshold float32) []PagePair {
	if m == nil {
		return nil
	}
	ps := m.pages()
	var ids []string
	for _, id := range ps.ids {
		if !IsSpecialPage(id) && (project == "" || ps.project[id] == project) {
			ids = append(ids, id)
		}
	}
	var out []PagePair
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			if s := dot(ps.vec[a], ps.vec[b]); s >= threshold {
				out = append(out, PagePair{ps.score(a, 0), ps.score(b, 0), s})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// BestPages ranks the wiki pages of a project by the best cosine of q with any of their chunks, best first, at most
// k. Index and Unsorted pages are left out.
func (m *Matrix) BestPages(q []float32, project string, k int) ([]PageScore, error) {
	if m == nil || len(m.IDs) == 0 || k <= 0 {
		return nil, nil
	}
	if len(q) != m.Dim {
		return nil, fmt.Errorf("query vector has %d dimensions, the index has %d: the embedder model changed; run `hms embed --reset`", len(q), m.Dim)
	}
	best := map[string]float32{}
	for i, id := range m.Node {
		if m.Kind[i] != "wiki" || (project != "" && m.Project[i] != project) || IsSpecialPage(id) {
			continue
		}
		s := dot(q, m.vecs[i*m.Dim:(i+1)*m.Dim])
		if old, ok := best[id]; !ok || s > old {
			best[id] = s
		}
	}
	ps := m.pages()
	var out []PageScore
	for id, s := range best {
		out = append(out, PageScore{NodeID: id, Project: ps.project[id], Title: pageTitle(id), Score: s})
		if out[len(out)-1].Project == "" {
			out[len(out)-1].Project = project
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].NodeID < out[j].NodeID
	})
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// MostSimilar returns the page of the project whose page vector is closest to q, other than skip. ok is false when
// there is none.
func (m *Matrix) MostSimilar(q []float32, project, skip string) (PageScore, bool) {
	if m == nil {
		return PageScore{}, false
	}
	ps := m.pages()
	q = ps.center(q)
	var best PageScore
	found := false
	for _, id := range ps.ids {
		if id == skip || IsSpecialPage(id) || ps.project[id] != project || len(ps.vec[id]) != len(q) {
			continue
		}
		if s := dot(q, ps.vec[id]); !found || s > best.Score {
			best, found = ps.score(id, s), true
		}
	}
	return best, found
}

// NodeVector is the L2-normalised mean of the vectors of the chunks of node id. A chunk without a stored vector is
// embedded with e (and not stored). It fails when the node has no chunks, or when a vector is needed and e is nil
// or fails.
func (d *DB) NodeVector(ctx context.Context, e Embedder, id string) ([]float32, error) {
	rows, err := d.Query(`SELECT c.text, c.text_hash FROM chunks c WHERE c.node_id=? ORDER BY c.seq`, id)
	if err != nil {
		return nil, err
	}
	type ch struct{ text, hash string }
	var chunks []ch
	for rows.Next() {
		var c ch
		if err := rows.Scan(&c.text, &c.hash); err != nil {
			rows.Close()
			return nil, err
		}
		chunks = append(chunks, c)
	}
	rows.Close()
	if len(chunks) == 0 {
		return nil, fmt.Errorf("%s has no chunks", id)
	}
	var sum []float64
	add := func(v []float32) error {
		if sum == nil {
			sum = make([]float64, len(v))
		}
		if len(v) != len(sum) {
			return fmt.Errorf("vectors of different sizes (%d and %d)", len(v), len(sum))
		}
		for i, x := range v {
			sum[i] += float64(x)
		}
		return nil
	}
	var missing []string
	for _, c := range chunks {
		var blob []byte
		var model string
		if e != nil {
			model = e.ID()
		}
		err := d.QueryRow(`SELECT vec FROM vec.vectors WHERE text_hash=? AND model=?`, c.hash, model).Scan(&blob)
		if err != nil {
			missing = append(missing, c.text)
			continue
		}
		if err := add(bytesf32(blob)); err != nil {
			return nil, err
		}
	}
	if len(missing) > 0 {
		if e == nil {
			return nil, fmt.Errorf("no embedder to embed the chunks of %s", id)
		}
		vs, err := e.Docs(ctx, missing)
		if err != nil {
			return nil, err
		}
		if len(vs) != len(missing) {
			return nil, fmt.Errorf("the embedder returned %d vectors for %d texts", len(vs), len(missing))
		}
		for _, v := range vs {
			if err := add(v); err != nil {
				return nil, err
			}
		}
	}
	v := unit64(sum)
	if v == nil {
		return nil, fmt.Errorf("%s has a zero vector", id)
	}
	return v, nil
}

// NodeVectorTimeout bounds the embedding done by callers that only need a vector for a moment.
const NodeVectorTimeout = 30 * time.Second

// ResolvePage finds the wiki page a path under wiki/ (project/Title.md, with or without the wiki/ prefix) or a title
// (Title or project/Title, any case, or an alias) names, as wiki_read does. It returns the node id.
func (d *DB) ResolvePage(in string) (string, error) {
	in = strings.Trim(strings.ReplaceAll(strings.TrimSpace(in), "\\", "/"), "/")
	in = strings.TrimPrefix(in, "wiki/")
	if in == "" {
		return "", fmt.Errorf("give a path under wiki/ (project/Title.md) or a page title")
	}
	if strings.HasSuffix(in, ".md") {
		id := "wiki/" + strings.TrimSuffix(in, ".md")
		var one int
		if d.QueryRow(`SELECT 1 FROM nodes WHERE id=? AND kind='wiki'`, id).Scan(&one) == nil {
			return id, nil
		}
	}
	id, matches, err := d.ResolveTitle(strings.TrimSuffix(in, ".md"))
	if err != nil {
		return "", err
	}
	switch {
	case id != "":
		return id, nil
	case len(matches) > 1:
		return "", fmt.Errorf("several pages answer to %q: %s; give the project as project/Title", in, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("no wiki entry %q", in)
}

// RelatedMin is the lowest cosine at which a page counts as related, in the space the page vectors are in.
func (m *Matrix) RelatedMin() float32 {
	if m != nil && m.pages().mean != nil {
		return RelatedFloor
	}
	return RelatedFloorRaw
}
