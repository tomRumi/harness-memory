package store

import (
	"context"
	"fmt"
	"github.com/tomRumi/harness-memory-service/internal/md"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
	"os"
	"path/filepath"
)

// WriteIndexPage regenerates wiki/<project>/Index.md from the pages in the index. The file is written (atomically)
// only when its content would change; the index is updated and the committer is told. It reports whether it wrote.
// An Index.md that the service did not generate (no `generated: true` in its frontmatter) is never overwritten: the
// error is ErrHandWritten.
func (s *Store) WriteIndexPage(ctx context.Context, proj string) (wrote bool, err error) {
	err = s.Do(func(tx *Tx) error {
		if err := tx.IndexPage(proj); err != nil {
			return err
		}
		wrote = len(tx.changed) > 0
		tx.Finish(ctx, "")
		return nil
	})
	return wrote && err == nil, err
}

// indexPageLocked builds the index page of a project and says whether it must be written: not when the file already
// has this content, and never over a hand-written Index.md. The caller holds the write lock.
func (s *Store) indexPageLocked(proj string) (rel, content string, wrote bool, err error) {
	if canon, err := wiki.Project(proj); err != nil || canon != proj { // never a second folder for a name that has capitals
		return "", "", false, fmt.Errorf("invalid project name %q", proj)
	}
	content, err = s.DB.BuildIndexPage(proj)
	if err != nil {
		return "", "", false, err
	}
	rel = "wiki/" + proj + "/" + wiki.IndexTitle + ".md"
	old, err := os.ReadFile(filepath.Join(s.Repo, filepath.FromSlash(rel)))
	switch {
	case err == nil:
		if string(old) == content {
			return rel, content, false, nil
		}
		if !md.Parse(old).Bool("generated") {
			return rel, content, false, ErrHandWritten
		}
	case !os.IsNotExist(err): // a file that cannot be read is not known to be the generated one: it is left alone
		return rel, content, false, fmt.Errorf("%w (cannot read it: %v)", ErrHandWritten, err)
	}
	return rel, content, true, nil
}
