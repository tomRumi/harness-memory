package index

import (
	"context"
	"sort"
	"strings"
)

// hitsByID fetches the chunks with the given ids as hits, keeping only active ones.
func (d *DB) hitsByID(ids []int64) (map[int64]Hit, error) {
	out := map[int64]Hit{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := d.Query(`SELECT c.id, n.id, n.kind, n.project, n.path, c.section, c.text FROM chunks c JOIN nodes n ON n.id=c.node_id
		WHERE n.status='active' AND c.id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ChunkID, &h.NodeID, &h.Kind, &h.Project, &h.Path, &h.Section, &h.Text); err != nil {
			return nil, err
		}
		out[h.ChunkID] = h
	}
	return out, rows.Err()
}

// Hybrid fuses the vector pass and the keyword pass by reciprocal rank: each chunk scores
// WeightVec/(RRFk+rank) for its place in the vector list plus WeightKw/(RRFk+rank) for its place in the keyword list.
// m may be nil or empty (no vectors yet): the result is then the keyword pass alone. A failing embedder also falls back
// to the keyword pass; the error is returned beside the hits so the caller can say so.
func (d *DB) Hybrid(ctx context.Context, e Embedder, m *Matrix, query string, f Filter) (hits []Hit, via string, warn error) {
	return d.HybridOpts(ctx, e, m, query, f, false)
}

// HybridOpts is Hybrid with the one-hop pass optional. With oneHop, up to HopSlots of the final places go to the best
// chunk of each entry that one of the top HopSeeds hits links to (ranked by similarity to the query), when that entry is
// not already in the result. At least 3 direct hits always remain, so the pass applies when the limit is 5 or more. The pass needs
// vectors: without them it does nothing.
func (d *DB) HybridOpts(ctx context.Context, e Embedder, m *Matrix, query string, f Filter, oneHop bool) (hits []Hit, via string, warn error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 5
	}
	kf := f
	kf.Limit = Candidates
	kw, err := d.Keyword(query, kf)
	if err != nil {
		return nil, "", err
	}
	var vecIDs []int64
	var qvec []float32
	if e != nil && m != nil && len(m.IDs) > 0 && strings.TrimSpace(query) != "" {
		q, qerr := e.Query(ctx, query)
		if qerr != nil {
			warn = qerr
		} else if vecIDs, qerr = m.TopK(q, Candidates, f); qerr != nil {
			warn = qerr
		} else {
			qvec = q
		}
	}
	score := map[int64]float64{}
	for r, h := range kw {
		score[h.ChunkID] += WeightKw / (RRFk + float64(r+1))
	}
	for r, id := range vecIDs {
		score[id] += WeightVec / (RRFk + float64(r+1))
	}
	ids := make([]int64, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > limit*3 {
		keep := map[int64]bool{}
		for r, h := range kw { // the protected keyword hits must survive the cut
			if r < ProtectKw {
				keep[h.ChunkID] = true
			}
		}
		cut := append([]int64(nil), ids[:limit*3]...) // a few more than needed, in case some are not active any more
		in := map[int64]bool{}
		for _, id := range cut {
			in[id] = true
		}
		for id := range keep {
			if !in[id] {
				cut = append(cut, id)
			}
		}
		ids = cut
	}
	byID, err := d.hitsByID(ids)
	if err != nil {
		return nil, "", err
	}
	var ordered []int64 // active chunks in fused order
	for _, id := range ids {
		if _, ok := byID[id]; ok {
			ordered = append(ordered, id)
		}
	}
	// The best keyword hits keep a place: a chunk that is only moderately close in both passes can otherwise outscore an
	// exact keyword match found by the keyword pass alone. They replace the lowest-ranked other places.
	protect := map[int64]bool{}
	for r, h := range kw {
		if r >= ProtectKw {
			break
		}
		if _, ok := byID[h.ChunkID]; ok {
			protect[h.ChunkID] = true
		}
	}
	final := ordered
	if len(final) > limit {
		final = append([]int64(nil), ordered[:limit]...)
		in := map[int64]bool{}
		for _, id := range final {
			in[id] = true
		}
		for _, h := range kw {
			if !protect[h.ChunkID] || in[h.ChunkID] {
				continue
			}
			for i := len(final) - 1; i >= 0; i-- { // drop the lowest-ranked place that is not itself protected
				if !protect[final[i]] {
					delete(in, final[i])
					final = append(final[:i], final[i+1:]...)
					break
				}
			}
			final = append(final, h.ChunkID)
			in[h.ChunkID] = true
		}
		sort.SliceStable(final, func(i, j int) bool {
			if score[final[i]] != score[final[j]] {
				return score[final[i]] > score[final[j]]
			}
			return final[i] < final[j]
		})
	}
	for _, id := range final {
		hh := byID[id]
		hh.Score = score[id]
		hits = append(hits, hh)
	}
	via = "keyword"
	if len(vecIDs) > 0 {
		via = "hybrid"
	}
	if oneHop && qvec != nil && limit >= HopSlots+3 && len(hits) > 0 {
		hits, err = d.addLinked(hits, m, qvec, f, limit)
		if err != nil {
			return nil, "", err
		}
		via += "+hop"
	}
	return hits, via, warn
}

// addLinked reserves up to HopSlots places at the end of hits for linked entries.
func (d *DB) addLinked(hits []Hit, m *Matrix, q []float32, f Filter, limit int) ([]Hit, error) {
	inResult := map[string]bool{}
	for _, h := range hits {
		inResult[h.NodeID] = true
	}
	type cand struct {
		chunk int64
		score float32
	}
	var cands []cand
	seen := map[string]bool{}
	for si, h := range hits {
		if si >= HopSeeds {
			break
		}
		rows, err := d.Query(`SELECT DISTINCT t.id FROM edges e JOIN nodes t ON t.status='active' AND (t.id = e.object_ref OR (t.project = ? AND t.id LIKE '%/' || e.object_ref))
			WHERE e.subject_id = ? AND e.relation = 'links_to' ORDER BY t.id LIMIT ?`, h.Project, h.NodeID, HopLinks)
		if err != nil {
			return nil, err
		}
		var linked []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			linked = append(linked, id)
		}
		rows.Close()
		for _, id := range linked {
			if inResult[id] || seen[id] {
				continue
			}
			seen[id] = true
			crows, err := d.Query(`SELECT c.id FROM chunks c JOIN nodes n ON n.id = c.node_id WHERE c.node_id = ?`+kindClause(f)+` ORDER BY c.seq`, kindArgs(id, f)...)
			if err != nil {
				return nil, err
			}
			best := cand{chunk: -1, score: -2}
			for crows.Next() {
				var cid int64
				if err := crows.Scan(&cid); err != nil {
					crows.Close()
					return nil, err
				}
				if sc, ok := m.Dot(q, cid); ok && sc > best.score {
					best = cand{cid, sc}
				}
			}
			crows.Close()
			if best.chunk >= 0 {
				cands = append(cands, best)
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > HopSlots {
		cands = cands[:HopSlots]
	}
	ids := make([]int64, len(cands))
	for i, c := range cands {
		ids[i] = c.chunk
	}
	byID, err := d.hitsByID(ids)
	if err != nil {
		return nil, err
	}
	var extra []Hit
	for _, c := range cands {
		if h, ok := byID[c.chunk]; ok {
			h.Score = float64(c.score)
			extra = append(extra, h)
		}
	}
	keep := limit - len(extra)
	if keep > len(hits) {
		keep = len(hits)
	}
	return append(hits[:keep:keep], extra...), nil
}

// kindClause and kindArgs apply the caller's project and kind filter to the linked entries as well.
func kindClause(f Filter) string {
	s := ""
	if f.Project != "" {
		s += " AND n.project = ?"
	}
	if len(f.Kinds) > 0 {
		s += " AND n.kind IN (" + strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",") + ")"
	}
	return s
}

func kindArgs(node string, f Filter) []any {
	a := []any{node}
	if f.Project != "" {
		a = append(a, f.Project)
	}
	for _, k := range f.Kinds {
		a = append(a, k)
	}
	return a
}
