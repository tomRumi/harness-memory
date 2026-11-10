package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory/internal/embed"
	"github.com/tomRumi/harness-memory/internal/index"
	"github.com/tomRumi/harness-memory/internal/store"
	"github.com/tomRumi/harness-memory/internal/wiki"
)

// Doctor runs the checks, prints one line each (OK, WARN or FAIL) and changes nothing. It returns the number of FAILs.
// A missing embedder is a WARN: keyword recall still works.
func Doctor(p Paths, out io.Writer) (fails int) {
	line := func(level, what, detail string) {
		if level == "FAIL" {
			fails++
		}
		fmt.Fprintf(out, "%-4s  %-22s %s\n", level, what, detail)
	}

	_, gitErr := exec.LookPath("git")
	if gitErr != nil {
		line("FAIL", "git program", "not found; install git (commits need it)")
	} else {
		line("OK", "git program", "found")
	}

	repoOK := false
	if _, err := os.Stat(filepath.Join(p.Repo, ".git")); err != nil {
		line("FAIL", "memory repository", p.Repo+" is not a git repository (run `hms init`)")
	} else {
		repoOK = true
		line("OK", "memory repository", p.Repo)
	}

	var db *index.DB
	if _, err := os.Stat(p.DB); err != nil {
		line("FAIL", "index", "no index at "+p.DB+" (run `hms init`)")
	} else if d, err := index.OpenReadOnly(p.DB); err != nil {
		line("FAIL", "index", err.Error())
	} else {
		db = d
		defer db.Close()
		line("OK", "index", p.DB+", schema matches")
	}

	doctorLinks(db, line)

	if !VectorsEnabled() {
		line("OK", "vectors", "off (HMS_VECTORS=0); embedder not checked")
	} else {
		doctorEmbedder(db, line)
	}

	if !repoOK || gitErr != nil {
		line("WARN", "commits", "skipped: no usable git repository")
	} else {
		doctorRepoState(p.Repo, line)
		doctorFolders(p.Repo, line)
	}
	return fails
}

// doctorLinks reports the links of wiki pages that lead nowhere or to more than one page.
func doctorLinks(db *index.DB, line func(level, what, detail string)) {
	if db == nil {
		line("WARN", "links", "skipped: no usable index")
		return
	}
	broken, ambiguous, err := db.LinkCounts()
	switch {
	case err != nil:
		line("WARN", "links", "cannot be counted: "+err.Error())
	case broken+ambiguous > 0:
		line("WARN", "links", fmt.Sprintf("%d broken, %d ambiguous (list them with `hms wiki links`)", broken, ambiguous))
	default:
		line("OK", "links", "0 broken, 0 ambiguous")
	}
}

func doctorEmbedder(db *index.DB, line func(level, what, detail string)) {
	cfg := embed.Default()
	c := embed.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := c.Check(ctx)
	switch st.State {
	case embed.Unreachable:
		line("WARN", "embedder", firstLine(st.Err)+"; keyword match only (to fix: install Ollama from https://ollama.com and run `ollama pull "+cfg.Model+"`, or set HMS_EMBED_URL)")
		line("WARN", "model", "not checked: no embedder")
	case embed.ModelMissing:
		line("OK", "embedder", "reachable at "+cfg.URL)
		line("WARN", "model", cfg.Model+" is missing on the server; run: ollama pull "+cfg.Model+" (keyword match only until then)")
	default:
		line("OK", "embedder", "reachable at "+cfg.URL)
		if cfg.API == "openai" {
			line("OK", "model", cfg.Model+": a test embedding works")
		} else {
			line("OK", "model", cfg.Model+" present")
		}
	}
	if db == nil {
		line("WARN", "vectors", "skipped: no usable index")
		return
	}
	var dims []int
	var verr error
	if _, err := os.Stat(strings.TrimSuffix(db.Path, ".db") + "-vectors.db"); err == nil {
		dims, verr = db.StoredDims(c.ID())
	}
	switch {
	case verr != nil:
		line("FAIL", "vector size", "cannot read stored vectors: "+verr.Error())
	case len(dims) > 1:
		line("FAIL", "vector size", fmt.Sprintf("stored vectors of %s have mixed sizes %v; run `hms embed -reset`", cfg.Model, dims))
	case len(dims) == 1 && st.State == embed.Ready && dims[0] != st.Dim:
		line("FAIL", "vector size", fmt.Sprintf("the embedder gives %d, stored vectors have %d; run `hms embed -reset`", st.Dim, dims[0]))
	case len(dims) == 0:
		line("OK", "vector size", "no stored vectors yet")
	default:
		line("OK", "vector size", fmt.Sprintf("%d, consistent", dims[0]))
	}
	embedded, total := 0, 0
	if len(dims) > 0 {
		embedded, total, _ = db.ChunkCounts(c.ID())
	} else {
		_, total, _ = db.ChunkCounts(c.ID())
	}
	if miss := total - embedded; miss > 0 {
		line("WARN", "chunks without vector", fmt.Sprintf("%d of %d (`hms serve` catches up, or run `hms embed`)", miss, total))
	} else {
		line("OK", "chunks without vector", fmt.Sprintf("0 of %d", total))
	}
}

// doctorRepoState checks what a running service would also see: no merge or rebase under way and nothing foreign
// staged. The last commit error of a running service is shown by its stats tool.
func doctorRepoState(repo string, line func(level, what, detail string)) {
	switch st := store.State(repo); {
	case st.IndexLock:
		line("WARN", "commits", fmt.Sprintf(".git/index.lock exists (%s old): every commit is blocked until it is removed; if no git program is running, delete that file", st.IndexLockAge))
	case st.MergeOrRebase:
		line("WARN", "commits", "the repository is in the middle of a merge or rebase; commits pause until it is finished")
	case st.Detached:
		line("WARN", "commits", "HEAD is detached; commits pause until a branch is checked out")
	case st.Err != nil:
		line("FAIL", "commits", "git diff --cached failed: "+st.Err.Error())
	case len(st.Foreign) > 0:
		line("WARN", "commits", fmt.Sprintf("%q is staged and is not a note, fact or wiki entry; commits wait until it is committed or unstaged", st.Foreign[0]))
	default:
		line("OK", "commits", "repository state fine (no merge or rebase, nothing foreign staged)")
	}
}

// doctorFolders warns about what makes the service refuse or lose a save: project folders whose name is not the
// canonical project name (capitals or invalid characters; the service writes the lower-case path, and git on a
// case-insensitive disk may not stage it), and notes, facts or wiki folders that git ignores (every save is refused).
func doctorFolders(repo string, line func(level, what, detail string)) {
	warned := false
	for _, tree := range wiki.Trees {
		for _, n := range store.OddFolders(repo, tree) {
			warned = true
			if c, err := wiki.Project(n); err == nil {
				line("WARN", "project folder", fmt.Sprintf("%s/%s has capital letters; project names are lower-case: git mv %s/%s %s/%s, then run hms rescan", tree, n, tree, n, tree, c))
			} else {
				line("WARN", "project folder", fmt.Sprintf("%s/%s is not a valid project name (lower-case letters, digits, dot, dash, underscore); rename it with git mv, then run hms rescan", tree, n))
			}
		}
		if _, err := store.Git(repo, "check-ignore", "-q", "--", tree+"/doctor-check/Sample.md"); err == nil {
			warned = true
			line("WARN", "ignored folder", fmt.Sprintf("git ignores %s/; every save is refused until the ignore rule is removed (see .gitignore, .git/info/exclude, the global ignore file)", tree))
		}
	}
	if !warned {
		line("OK", "memory folders", "project folders are lower-case, notes, facts and wiki are not ignored by git")
	}
}
