package store

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory/internal/wiki"
)

// RepoState says whether the repository lets commits through. The committer and `hms doctor` ask this one place.
// Only the first thing that blocks is reported, in this order: index.lock, a merge or rebase, a detached HEAD, a
// staged file that is not a note, fact or wiki entry.
type RepoState struct {
	IndexLock     bool          // .git/index.lock exists
	IndexLockAge  time.Duration // how old it is
	MergeOrRebase bool
	Detached      bool
	Foreign       []string // staged paths outside notes/, facts/ and wiki/
	Err           error    // git could not say what is staged
}

// State reads the state of the repository.
func State(repo string) RepoState {
	g := filepath.Join(repo, ".git")
	var st RepoState
	if fi, err := os.Stat(filepath.Join(g, "index.lock")); err == nil {
		st.IndexLock, st.IndexLockAge = true, time.Since(fi.ModTime()).Round(time.Second)
	} else if mergeInProgress(repo) {
		st.MergeOrRebase = true
	} else if _, err := os.Stat(g); err == nil && detached(repo) {
		st.Detached = true
	} else if out, err := Git(repo, "diff", "--cached", "--name-only", "-z"); err != nil {
		st.Err = err
	} else {
		st.Foreign = foreignStaged(out)
	}
	return st
}

// mergeInProgress reports whether git is in the middle of a merge, a rebase or a cherry-pick.
func mergeInProgress(repo string) bool {
	for _, f := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD"} {
		if _, err := os.Stat(filepath.Join(repo, ".git", f)); err == nil {
			return true
		}
	}
	return false
}

// detached reports whether HEAD points at no branch. An unborn branch (no commit yet) is a branch.
func detached(repo string) bool {
	_, err := Git(repo, "symbolic-ref", "-q", "HEAD")
	return err != nil
}

// foreignStaged picks, from the NUL separated output of `git diff --cached --name-only -z`, the paths that are not
// notes, facts or wiki entries: the owner has staged them, and a memory commit would sweep them in.
func foreignStaged(out string) []string {
	var foreign []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" && !wiki.InTree(f) {
			foreign = append(foreign, f)
		}
	}
	return foreign
}
