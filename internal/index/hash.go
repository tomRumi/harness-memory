package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
)

// StateHash is a digest of everything the index derives from files: nodes, chunks (by text hash) and edges.
// Row ids, timestamps of the run and the events table are left out, so two rebuilds from the same files agree.
func (d *DB) StateHash() (string, map[string]int, error) {
	h := sha256.New()
	counts := map[string]int{}
	feed := func(table, q string, ncols int) error {
		rows, err := d.Query(q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			vals := make([]any, ncols)
			ptrs := make([]any, ncols)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			fmt.Fprintf(h, "%s", table)
			for _, v := range vals {
				fmt.Fprintf(h, "\x1f%v", v)
			}
			h.Write([]byte{'\n'})
			counts[table]++
		}
		return rows.Err()
	}
	if err := feed("node", `SELECT id,kind,project,path,title,pinned,note_kind,created_at,updated_at,content_hash FROM nodes ORDER BY id`, 10); err != nil {
		return "", nil, err
	}
	if err := feed("chunk", `SELECT node_id,seq,section,text_hash FROM chunks ORDER BY node_id,seq`, 4); err != nil {
		return "", nil, err
	}
	if err := feed("edge", `SELECT subject_id,relation,object_ref,source,resolved,status FROM edges ORDER BY subject_id,relation,object_ref,source`, 6); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), counts, nil
}

// NodeDigests returns one digest per node over its row, its chunks and its edges. Comparing two maps shows
// exactly which nodes differ between two index states.
func (d *DB) NodeDigests() (map[string]string, error) {
	out := map[string]hash.Hash{}
	get := func(id string) hash.Hash {
		if out[id] == nil {
			out[id] = sha256.New()
		}
		return out[id]
	}
	scan := func(q string, ncols int, tag string) error {
		rows, err := d.Query(q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			vals := make([]any, ncols)
			ptrs := make([]any, ncols)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			h := get(fmt.Sprint(vals[0]))
			fmt.Fprint(h, tag)
			for _, v := range vals[1:] {
				fmt.Fprintf(h, "\x1f%v", v)
			}
			h.Write([]byte{'\n'})
		}
		return rows.Err()
	}
	if err := scan(`SELECT id,kind,project,path,title,pinned,note_kind,created_at,updated_at,content_hash FROM nodes ORDER BY id`, 10, "n"); err != nil {
		return nil, err
	}
	if err := scan(`SELECT node_id,seq,section,text_hash FROM chunks ORDER BY node_id,seq`, 4, "c"); err != nil {
		return nil, err
	}
	if err := scan(`SELECT subject_id,relation,object_ref,source,resolved,status FROM edges ORDER BY subject_id,relation,object_ref,source`, 6, "e"); err != nil {
		return nil, err
	}
	res := map[string]string{}
	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		res[id] = hex.EncodeToString(out[id].Sum(nil))
	}
	return res, nil
}
