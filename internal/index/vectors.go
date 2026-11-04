package index

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"
)

// Embedder turns text into unit-length vectors. internal/embed.Client is the implementation.
type Embedder interface {
	Model() string
	Docs(ctx context.Context, texts []string) ([][]float32, error)
	Query(ctx context.Context, text string) ([]float32, error)
}

// Frozen retrieval settings. They are constants on purpose: they are fixed and committed before a scored run,
// and a change to any of them is a new commit with a new score, not a runtime option.
const (
	RRFk        = 60.0 // reciprocal rank fusion constant
	WeightVec   = 1.0  // weight of the vector pass
	WeightKw    = 1.0  // weight of the keyword pass
	Candidates  = 50   // chunks taken from each pass before fusion
	ProtectKw   = 2    // the best keyword hits always keep a place in the result (settings v2, see evidence/step-2.md)
	embedBatch  = 8
	embedWorker = 3
)

func f32bytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func bytesf32(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

type pending struct{ hash, text string }

// vectorKinds are the kinds of node that get vectors (settings v3). Code is left to the keyword pass: identifiers are
// lexical, a general text model embeds source code poorly, and code is 80% of the chunks, so leaving it out cuts the
// embedding work to a fifth. Measured on the development sets: vectors never won on code and lost two questions.
const vectorKinds = `('wiki','note','fact')`

// layerOrder puts the layers people ask about first: wiki, then notes and facts, then code.
const layerOrder = `CASE n.kind WHEN 'wiki' THEN 0 WHEN 'code' THEN 2 ELSE 1 END`

// Pending counts the distinct chunk texts that have no vector for model yet, and the distinct texts in all.
func (d *DB) Pending(model string) (missing, total int, err error) {
	err = d.QueryRow(`SELECT count(DISTINCT c.text_hash) FROM chunks c JOIN nodes n ON n.id=c.node_id WHERE n.kind IN ` + vectorKinds).Scan(&total)
	if err != nil {
		return
	}
	err = d.QueryRow(`SELECT count(DISTINCT c.text_hash) FROM chunks c JOIN nodes n ON n.id=c.node_id
		WHERE n.kind IN `+vectorKinds+` AND NOT EXISTS (SELECT 1 FROM vec.vectors v WHERE v.text_hash=c.text_hash AND v.model=?)`, model).Scan(&missing)
	return
}

// EmbedPending embeds every chunk text that has no vector for the embedder's model, wiki first and code last.
// It is safe to stop and run again: each batch is stored as soon as it arrives. progress may be nil.
func (d *DB) EmbedPending(ctx context.Context, e Embedder, progress func(done, total int)) (int, error) {
	model := e.Model()
	rows, err := d.Query(`SELECT c.text_hash, c.text, `+layerOrder+` AS pri FROM chunks c JOIN nodes n ON n.id=c.node_id
		WHERE n.kind IN `+vectorKinds+` AND NOT EXISTS (SELECT 1 FROM vec.vectors v WHERE v.text_hash=c.text_hash AND v.model=?)
		ORDER BY pri, c.id`, model)
	if err != nil {
		return 0, err
	}
	var todo []pending
	seen := map[string]bool{}
	for rows.Next() {
		var p pending
		var pri int
		if err := rows.Scan(&p.hash, &p.text, &pri); err != nil {
			rows.Close()
			return 0, err
		}
		if !seen[p.hash] {
			seen[p.hash] = true
			todo = append(todo, p)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(todo) == 0 {
		return 0, nil
	}
	type batch struct{ items []pending }
	type result struct {
		items []pending
		vecs  [][]float32
		err   error
	}
	jobs := make(chan batch)
	out := make(chan result)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < embedWorker; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				texts := make([]string, len(b.items))
				for i, it := range b.items {
					texts[i] = it.text
				}
				v, err := e.Docs(ctx, texts)
				select {
				case out <- result{b.items, v, err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := 0; i < len(todo); i += embedBatch {
			j := i + embedBatch
			if j > len(todo) {
				j = len(todo)
			}
			select {
			case jobs <- batch{todo[i:j]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(out) }()

	done := 0
	var firstErr error
	for r := range out {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				cancel()
			}
			continue
		}
		if firstErr != nil {
			continue
		}
		tx, err := d.Begin()
		if err != nil {
			return done, err
		}
		for i, it := range r.items {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO vec.vectors(text_hash, model, dim, vec) VALUES(?,?,?,?)`,
				it.hash, model, len(r.vecs[i]), f32bytes(r.vecs[i])); err != nil {
				tx.Rollback()
				return done, err
			}
		}
		if err := tx.Commit(); err != nil {
			return done, err
		}
		done += len(r.items)
		if progress != nil {
			progress(done, len(todo))
		}
	}
	return done, firstErr
}

// DropVectors removes the cached vectors of every model except keep (use "" to remove them all), and the vectors of
// texts that no wiki, note or fact chunk uses any more (including every code vector). It returns how many were removed.
func (d *DB) DropVectors(keep string) (int, error) {
	res, err := d.Exec(`DELETE FROM vec.vectors WHERE model <> ? OR text_hash NOT IN
		(SELECT c.text_hash FROM chunks c JOIN nodes n ON n.id=c.node_id WHERE n.kind IN `+vectorKinds+`)`, keep)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Matrix holds the active chunks that have a vector, in memory, for a dot-product search.
type Matrix struct {
	Dim     int
	IDs     []int64
	Kind    []string
	Project []string
	vecs    []float32
	Missing int // active wiki, note and fact chunks that have no vector yet
}

// LoadMatrix reads every vector of model for active chunks. It fails if the cache mixes dimensions.
func (d *DB) LoadMatrix(model string) (*Matrix, error) {
	m := &Matrix{}
	rows, err := d.Query(`SELECT c.id, n.kind, n.project, v.dim, v.vec FROM chunks c JOIN nodes n ON n.id=c.node_id
		JOIN vec.vectors v ON v.text_hash=c.text_hash AND v.model=? WHERE n.status='active' AND n.kind IN `+vectorKinds+` ORDER BY c.id`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind, proj string
		var dim int
		var blob []byte
		if err := rows.Scan(&id, &kind, &proj, &dim, &blob); err != nil {
			return nil, err
		}
		if m.Dim == 0 {
			m.Dim = dim
		}
		if dim != m.Dim || len(blob) != 4*dim {
			return nil, fmt.Errorf("vector cache for %s mixes dimensions (%d and %d); run `hms embed --reset`", model, m.Dim, dim)
		}
		m.IDs = append(m.IDs, id)
		m.Kind = append(m.Kind, kind)
		m.Project = append(m.Project, proj)
		m.vecs = append(m.vecs, bytesf32(blob)...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var active int
	if err := d.QueryRow(`SELECT count(*) FROM chunks c JOIN nodes n ON n.id=c.node_id WHERE n.status='active' AND n.kind IN ` + vectorKinds).Scan(&active); err != nil {
		return nil, err
	}
	m.Missing = active - len(m.IDs)
	return m, nil
}

type scored struct {
	idx   int
	score float32
}

// TopK returns the chunk ids with the highest dot product with q that pass the filter, best first.
func (m *Matrix) TopK(q []float32, k int, f Filter) ([]int64, error) {
	if len(m.IDs) == 0 {
		return nil, nil
	}
	if len(q) != m.Dim {
		return nil, fmt.Errorf("query vector has %d dimensions, the index has %d: the embedder model changed; run `hms embed --reset`", len(q), m.Dim)
	}
	kinds := map[string]bool{}
	for _, kd := range f.Kinds {
		kinds[kd] = true
	}
	var best []scored
	for i := range m.IDs {
		if f.Project != "" && m.Project[i] != f.Project {
			continue
		}
		if len(kinds) > 0 && !kinds[m.Kind[i]] {
			continue
		}
		row := m.vecs[i*m.Dim : (i+1)*m.Dim]
		var s float32
		for j, x := range row {
			s += x * q[j]
		}
		if len(best) < k || s > best[len(best)-1].score {
			best = append(best, scored{i, s})
			sort.SliceStable(best, func(a, b int) bool {
				if best[a].score != best[b].score {
					return best[a].score > best[b].score
				}
				return m.IDs[best[a].idx] < m.IDs[best[b].idx]
			})
			if len(best) > k {
				best = best[:k]
			}
		}
	}
	ids := make([]int64, len(best))
	for i, b := range best {
		ids[i] = m.IDs[b.idx]
	}
	return ids, nil
}
