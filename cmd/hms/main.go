// Command hms is the command line of harness-memory-service.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tomRumi/harness-memory-service/internal/curate"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/store"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
)

const usage = `hms: markdown-backed memory with a rebuildable index

usage: hms [--home DIR] [--repo DIR] [--db FILE] <command> [arguments]

  init [--yes]                 create the memory repository (with git) and the index; check the embedder and offer to
                               download the model (--yes answers yes)
  doctor                       check git, the repository, the index and the embedder; changes nothing; exit 1 on a FAIL
  rescan                       make the index match the files (changed files only)
  embed [-reset] [-prune]      embed every wiki, note and fact chunk that has no vector yet; safe to stop and rerun
  recall [-p P] [-k K,K] [-n N] [-vectors=false] [-json] <words...>
                               recall; keyword and vector search together (HMS_VECTORS=0 or -vectors=false: keywords only)
  bench <questions.json> ...   recall latency over the questions' texts (query vector cached, then with embedding)
  serve [-addr A] [-vectors=false]  MCP over HTTP (loopback only) with seven tools: recall learn wiki_read fact_add retire feedback stats
  wiki links [-p P]            list broken and ambiguous [[links]] of wiki pages (one line each, then a summary); exit 0
  wiki index [-p P]            write wiki/<project>/Index.md (all projects without -p) when its content changed, and commit it
  wiki related <page> [-n 5] [-p P]  the pages closest in meaning to a page (title or path): score, [[Title]], summary
  wiki duplicates [-p P] [-t T]  pairs of pages that look like copies (cosine of page vectors >= T, default 0.92)
  wiki eval-pairs -pairs FILE  score related, unrelated and duplicate page pairs against the current vectors; read only; exit 0
  curate                       notes waiting per project and the age of the oldest (read only)
  curate -pending [-p P] [-max N]  the batches to merge as JSON: notes, page titles, related pages, instructions
  curate -submit FILE|- [-p P] check and write the pages of a model answer; one commit; exit 1 on error
  curate -apply [-p P] [-max N]    the same with the built-in model (HMS_LLM_URL, HMS_LLM_MODEL)
  stats                        node counts by kind and project, and the vector state
  hash                         digest of everything derived from files
  digests                      one digest per node, as JSON

HMS_HOME, HMS_REPO and HMS_DB set the same locations. The embedding model is set with HMS_EMBED_API (ollama or openai),
HMS_EMBED_URL, HMS_EMBED_MODEL and HMS_EMBED_KEY; the default is nomic-embed-text on a local Ollama. Default home: $XDG_DATA_HOME/harness-memory-service.
`

type paths struct{ home, repo, db string }

func resolve(home, repo, db string) paths {
	if home == "" {
		home = os.Getenv("HMS_HOME")
	}
	if home == "" {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			h, _ := os.UserHomeDir()
			base = filepath.Join(h, ".local", "share")
		}
		home = filepath.Join(base, "harness-memory-service")
	}
	if repo == "" {
		repo = os.Getenv("HMS_REPO")
	}
	if repo == "" {
		repo = filepath.Join(home, "memory")
	}
	if db == "" {
		db = os.Getenv("HMS_DB")
	}
	if db == "" {
		db = filepath.Join(home, "index.db")
	}
	return paths{home, repo, db}
}

// projectArg applies the project rule to a -p value in place (empty: all projects). It prints why and says false when
// the name cannot be a project.
func projectArg(v *string, stderr io.Writer) bool {
	p, err := wiki.OptionalProject(*v)
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return false
	}
	*v = p
	return true
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "hms: "+format+"\n", a...)
	os.Exit(1)
}

// commands maps a command name to the function that runs it with the arguments after the name.
var commands = map[string]func(p paths, rest []string){
	"init": cmdInit, "doctor": cmdDoctor, "rescan": cmdRescan, "recall": cmdRecall, "embed": cmdEmbed,
	"bench": cmdBench, "serve": cmdServe, "stats": cmdStats, "hash": cmdHash, "digests": cmdDigests,
	"wiki":   func(p paths, rest []string) { os.Exit(wikiCmd(p, rest, os.Stdout, os.Stderr)) },
	"curate": func(p paths, rest []string) { os.Exit(curateCmd(p, rest, os.Stdin, os.Stdout, os.Stderr)) },
}

func main() {
	gf := flag.NewFlagSet("hms", flag.ExitOnError)
	home := gf.String("home", "", "data directory")
	repo := gf.String("repo", "", "memory repository directory")
	db := gf.String("db", "", "index file")
	gf.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	gf.Parse(os.Args[1:])
	args := gf.Args()
	var run func(paths, []string)
	if len(args) > 0 {
		run = commands[args[0]]
	}
	if run == nil {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	run(resolve(*home, *repo, *db), args[1:])
}

func open(p paths) *index.DB {
	d, err := index.Open(p.db)
	if err != nil {
		die("open index %s: %v", p.db, err)
	}
	return d
}

// openRescanned opens the index and makes it match the files, so that a command reports what is on disk now. It
// prints why and returns a nil index when that fails; the second result is the exit status then.
func openRescanned(p paths, stderr io.Writer) (*index.DB, int) {
	d, err := index.Open(p.db)
	if err != nil {
		fmt.Fprintf(stderr, "hms: open index %s: %v\n", p.db, err)
		return nil, 1
	}
	res, err := d.Rescan(p.repo)
	if err != nil {
		d.Close()
		fmt.Fprintf(stderr, "hms: rescan: %v\n", err)
		return nil, 1
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	return d, 0
}

// openStore builds the write side the way every command that writes needs it: the committer, the store (events.db
// lives in the home) and, when sweep is set, the notes and pages an earlier run left uncommitted. The caller closes
// the store (closeStore, or st.Close in a service).
func openStore(p paths, d *index.DB, e index.Embedder, sweep bool, stderr io.Writer) (*store.Store, error) {
	st, err := store.New(p.repo, d, e, store.NewCommitter(p.repo, store.QuietDelay), filepath.Join(p.home, "events.db"))
	if err != nil {
		return nil, err
	}
	if sweep {
		sweepLeftovers(st, stderr)
	}
	return st, nil
}

// sweepLeftovers commits the notes and pages that an earlier run left uncommitted or staged (it was killed between
// writing and committing). Every command that writes does it first, so the files never wait for a serve to be started.
// It holds the write lock (see Store.Sweep).
func sweepLeftovers(st *store.Store, stderr io.Writer) {
	store.CleanTemp(st.Repo)
	if err := st.Sweep(); err != nil {
		fmt.Fprintf(stderr, "hms: sweep: %v\n", err)
	}
}

// closeStore makes the last commit. When it failed, the files are saved but not committed: say so and make the exit
// status 1, so a script or a harness does not go on as if the commit had been made.
func closeStore(st *store.Store, stderr io.Writer, code *int) {
	st.Close()
	if err := st.Commit.LastErr(); err != nil {
		fmt.Fprintln(stderr, "hms:", curate.CommitFailed(err))
		*code = 1
	}
}
