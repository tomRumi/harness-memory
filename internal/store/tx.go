package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Tx is what a multi-file operation (the merge) may do while it holds the write lock: write and delete files and
// regenerate the index page, each with the index entry updated at once. The committer is told once, by Finish.
// Every file it touches is remembered as it was; if the operation fails, Do puts all of them back.
type Tx struct {
	s       *Store
	changed []string
	seen    map[string]bool
	undo    []undoEntry
	undone  map[string]bool
	events  []txEvent // recorded in events.db only when the operation succeeds
}

type txEvent struct{ kind, node, project, detail string }

// undoEntry is a file as it was before the first change of the operation: its bytes, or its absence.
type undoEntry struct {
	rel     string
	data    []byte
	existed bool
}

// Do runs fn with the write lock held (the mutex of this process and the lock file of the memory home, so another
// process cannot write meanwhile). If fn returns an error or panics, every file it changed is restored, the index
// entries with them, and nothing is committed.
func (s *Store) Do(fn func(tx *Tx) error) (err error) {
	unlock, err := s.lockWrite()
	if err != nil {
		return err
	}
	defer unlock()
	tx := &Tx{s: s, seen: map[string]bool{}, undone: map[string]bool{}}
	defer func() {
		r := recover()
		if r == nil && err == nil {
			return
		}
		if rerr := tx.rollback(); rerr != nil {
			if err == nil {
				err = fmt.Errorf("panic: %v", r)
			}
			err = fmt.Errorf("%w (and putting the files back failed: %v)", err, rerr)
		}
		if r != nil {
			panic(r)
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	for _, e := range tx.events {
		s.Event(e.kind, e.node, e.project, e.detail)
	}
	return nil
}

// remember records rel as it is now, once. A file that exists but cannot be read is an error, never "missing":
// putting it back would be impossible, so it is not touched.
func (tx *Tx) remember(rel string) error {
	if tx.undone[rel] {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(tx.s.Repo, filepath.FromSlash(rel)))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot read %s, so it is not changed: %w", rel, err)
	}
	tx.undone[rel] = true
	tx.undo = append(tx.undo, undoEntry{rel: rel, data: data, existed: err == nil})
	return nil
}

// rollback restores every remembered file, newest first, and the index entries. It returns the first failure after
// trying all of them.
func (tx *Tx) rollback() error {
	var errs []error
	for i := len(tx.undo) - 1; i >= 0; i-- {
		u := tx.undo[i]
		full := filepath.Join(tx.s.Repo, filepath.FromSlash(u.rel))
		cur, err := os.ReadFile(full)
		switch {
		case u.existed && err == nil && string(cur) == string(u.data):
			continue // never changed (the failed step did not get that far)
		case !u.existed && err != nil:
			continue
		case u.existed:
			err = writeFile(full, u.data)
		default:
			err = os.Remove(full)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if err := tx.s.DB.Upsert(tx.s.Repo, u.rel); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
	if err := tx.remember(rel); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(tx.s.Repo, filepath.FromSlash(rel)), data); err != nil {
		return err
	}
	tx.note(rel)
	return tx.s.DB.Upsert(tx.s.Repo, rel)
}

// Delete removes the note or fact node id (for example notes/p/<uuid>) and updates the index. A non-empty event
// kind is recorded in the event history with the detail.
func (tx *Tx) Delete(id, project, event, detail string) error {
	if err := tx.remember(id + ".md"); err != nil {
		return err
	}
	rel, err := tx.s.removeNode(id, project, "", "")
	if err != nil {
		return err
	}
	if event != "" {
		tx.events = append(tx.events, txEvent{event, id, project, detail})
	}
	tx.note(rel)
	return tx.s.DB.Upsert(tx.s.Repo, rel)
}

// IndexPage regenerates wiki/<project>/Index.md from the index as it is now. It returns ErrHandWritten, and writes
// nothing, when the project has an Index.md that the service did not generate.
func (tx *Tx) IndexPage(project string) error {
	rel, content, wrote, err := tx.s.indexPageLocked(project)
	if err != nil || !wrote {
		return err
	}
	return tx.Write(rel, []byte(content))
}

// Busy reports whether git is in the middle of a merge or a rebase.
func (tx *Tx) Busy() bool { return tx.s.Commit != nil && tx.s.Commit.Busy() }

// Finish tells the committer about every file of the operation in one Notify with the commit message; embedding of
// what is new follows in the background, after the lock is released. With no changed file it does nothing.
func (tx *Tx) Finish(ctx context.Context, msg string) {
	if len(tx.changed) == 0 {
		return
	}
	tx.s.finish(ctx, msg, tx.changed...)
}
