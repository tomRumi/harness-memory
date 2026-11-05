package index

import (
	"context"
	"sort"
	"strings"
	"time"
)

// hitsByID fetches the chunks with the given ids as hits.
func (d *DB) hitsByID(ids []int64) (map[int64]Hit, error) {
	out := map[int64]Hit{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `SELECT c.id, n.id, n.kind, n.project, n.path, c.section, c.text FROM chunks c JOIN nodes n ON n.id=c.node_id WHERE c.id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	rows, err := d.Query(q, args...)
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
	if e != nil && m != nil && len(m.IDs) > 0 && strings.TrimSpace(query) != "" && wantsVectors(f) {
		qctx, cancel := context.WithTimeout(ctx, QueryEmbedTimeout)
		defer cancel()
		q, qerr := e.Query(qctx, query)
		if qerr != nil {
			warn = qerr
		} else if vecIDs, qerr = m.TopK(q, Candidates, f); qerr != nil {
			warn = qerr
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
		cut := append([]int64(nil), ids[:limit*3]...) // a few more than needed
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
	var ordered []int64 // chunks in fused order
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
	return hits, via, warn
}

// QueryEmbedTimeout bounds the embedding of a query. A hung model server must not stall a recall: after this the
// keyword result is returned.
const QueryEmbedTimeout = 2 * time.Second

// wantsVectors is false when the filter asks only for kinds that have no vectors: no vector can match, so the
// model call is skipped.
func wantsVectors(f Filter) bool {
	if len(f.Kinds) == 0 {
		return true
	}
	for _, k := range f.Kinds {
		if k == "wiki" || k == "note" || k == "fact" {
			return true
		}
	}
	return false
}
