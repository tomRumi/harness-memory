package store

import (
	"context"
	"path/filepath"
)

// Tx is what a multi-file operation (the merge) may do while it holds the write lock: write and delete files and
// regenerate the index page, each with the index entry updated at once. The committer is told once, by Finish.
type Tx struct {
	s       *Store
	changed []string
	seen    map[string]bool
}

// Do runs fn with the write lock held. Nothing else writes to the repository meanwhile.
func (s *Store) Do(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&Tx{s: s, seen: map[string]bool{}})
}

// Repo is the repository directory.
func (tx *Tx) Repo() string { return tx.s.Repo }

// Changed lists the files written or deleted so far, in order.
func (tx *Tx) Changed() []string { return append([]string(nil), tx.changed...) }

func (tx *Tx) note(rel string) {
	if !tx.seen[rel] {
		tx.seen[rel] = true
		tx.changed = append(tx.changed, rel)
	}
}

// Write writes rel (a path under the repository) atomically and updates its index entry.
func (tx *Tx) Write(rel string, data []byte) error {
	if err := writeFile(filepath.Join(tx.s.Repo, filepath.FromSlash(rel)), data); err != nil {
		return err
	}
	tx.note(rel)
	return tx.s.DB.Upsert(tx.s.Repo, rel)
}

// Delete removes the note or fact node id (for example notes/p/<uuid>) and updates the index. A non-empty event
// kind is recorded in the event history with the detail.
func (tx *Tx) Delete(id, project, event, detail string) error {
	rel, err := tx.s.removeNode(id, project, event, detail)
	if err != nil {
		return err
	}
	tx.note(rel)
	return tx.s.DB.Upsert(tx.s.Repo, rel)
}

// IndexPage regenerates wiki/<project>/Index.md from the index as it is now.
func (tx *Tx) IndexPage(project string) error {
	rel, wrote, err := tx.s.writeIndexPageLocked(project)
	if err != nil {
		return err
	}
	if wrote {
		tx.note(rel)
	}
	return nil
}

// Busy reports whether git is in the middle of a merge or a rebase.
func (tx *Tx) Busy() bool { return tx.s.Commit != nil && tx.s.Commit.Busy() }

// Finish tells the committer about every file of the operation in one Notify with the commit message, then embeds
// what is new (best effort). With no changed file it does nothing.
func (tx *Tx) Finish(ctx context.Context, msg string) {
	if len(tx.changed) == 0 {
		return
	}
	tx.s.finish(ctx, msg, tx.changed...)
}
