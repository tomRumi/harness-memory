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

	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
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

	if !VectorsEnabled() {
		line("OK", "vectors", "off (HMS_VECTORS=0); embedder not checked")
	} else {
		doctorEmbedder(db, line)
	}

	if !repoOK || gitErr != nil {
		line("WARN", "commits", "skipped: no usable git repository")
	} else {
		doctorRepoState(p.Repo, line)
	}
	return fails
}

func doctorEmbedder(db *index.DB, line func(level, what, detail string)) {
	cfg := embed.Default()
	c := embed.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := c.Check(ctx)
	switch st.State {
	case embed.Unreachable:
		line("WARN", "embedder", firstLine(st.Err)+"; keyword match only. "+strings.TrimPrefix(NoEmbedderLine(cfg), "vectors: unavailable: "))
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
	g := filepath.Join(repo, ".git")
	for _, f := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD"} {
		if _, err := os.Stat(filepath.Join(g, f)); err == nil {
			line("WARN", "commits", "the repository is in the middle of a merge or rebase; commits pause until it is finished")
			return
		}
	}
	out, err := exec.Command("git", "-C", repo, "diff", "--cached", "--name-only").Output()
	if err != nil {
		line("FAIL", "commits", "git diff --cached failed: "+err.Error())
		return
	}
	var foreign []string
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f != "" && !strings.HasPrefix(f, "notes/") && !strings.HasPrefix(f, "facts/") && !strings.HasPrefix(f, "wiki/") {
			foreign = append(foreign, f)
		}
	}
	if len(foreign) > 0 {
		line("WARN", "commits", fmt.Sprintf("%q is staged and is not a note, fact or wiki entry; commits wait until it is committed or unstaged", foreign[0]))
		return
	}
	line("OK", "commits", "repository state fine (no merge or rebase, nothing foreign staged)")
}
