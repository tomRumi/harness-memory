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
	if e != nil && m != nil && len(m.IDs) > 0 && strings.TrimSpace(query) != "" {
		q, qerr := e.Query(ctx, query)
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
		ids = ids[:limit*3] // fetch a few more than needed in case some are not active any more
	}
	byID, err := d.hitsByID(ids)
	if err != nil {
		return nil, "", err
	}
	for _, id := range ids {
		if h, ok := byID[id]; ok {
			h.Score = score[id]
			hits = append(hits, h)
			if len(hits) == limit {
				break
			}
		}
	}
	via = "keyword"
	if len(vecIDs) > 0 {
		via = "hybrid"
	}
	return hits, via, warn
}
