// Command hms is the command line of harness-memory-service.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
)

const usage = `hms: markdown-backed memory with a rebuildable index

usage: hms [--home DIR] [--repo DIR] [--db FILE] <command> [arguments]

  init                         create the memory repository and the index
  rescan                       make the index match the files (changed files only)
  embed [-reset]               embed every chunk that has no vector yet (wiki first, code last); safe to stop and rerun
  recall [-p P] [-k K,K] [-n N] [-keyword] [-hop] [-json] <words...>
                               hybrid recall (vectors + keywords) over active nodes; -keyword skips the vectors
  stats                        node counts by kind and project
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

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "hms: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	gf := flag.NewFlagSet("hms", flag.ExitOnError)
	home := gf.String("home", "", "data directory")
	repo := gf.String("repo", "", "memory repository directory")
	db := gf.String("db", "", "index file")
	gf.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	gf.Parse(os.Args[1:])
	args := gf.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	p := resolve(*home, *repo, *db)
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		cmdInit(p)
	case "rescan":
		d := open(p)
		defer d.Close()
		res, err := d.Rescan(p.repo)
		if err != nil {
			die("rescan: %v", err)
		}
		fmt.Printf("added %d  changed %d  removed %d  unchanged %d\n", res.Added, res.Changed, res.Removed, res.Unchanged)
		for _, id := range res.ChangedIDs {
			fmt.Println("changed:", id)
		}
		for _, w := range res.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
	case "recall":
		fs := flag.NewFlagSet("recall", flag.ExitOnError)
		proj := fs.String("p", "", "project")
		kinds := fs.String("k", "", "kinds, comma separated: wiki,code,note,fact")
		n := fs.Int("n", 5, "number of hits")
		asJSON := fs.Bool("json", false, "print hits as JSON, with the full text")
		kwOnly := fs.Bool("keyword", false, "keyword pass only")
		hop := fs.Bool("hop", false, "reserve places for entries that the top hits link to (needs vectors)")
		fs.Parse(rest)
		d := open(p)
		defer d.Close()
		f := index.Filter{Project: *proj, Limit: *n}
		if *kinds != "" {
			f.Kinds = strings.Split(*kinds, ",")
		}
		query := strings.Join(fs.Args(), " ")
		var hits []index.Hit
		var err error
		if *kwOnly {
			hits, err = d.Keyword(query, f)
		} else {
			e := embed.New(embed.Default())
			m, merr := d.LoadMatrix(e.Model())
			if merr != nil {
				die("%v", merr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var via string
			var warn error
			hits, via, warn = d.HybridOpts(ctx, e, m, query, f, *hop)
			if warn != nil {
				fmt.Fprintln(os.Stderr, "hms: vector pass skipped:", warn)
			}
			if m.Missing > 0 {
				fmt.Fprintf(os.Stderr, "hms: %d chunks have no vector yet (run `hms embed`); %s result\n", m.Missing, via)
			}
		}
		if err != nil {
			die("%v", err)
		}
		if *asJSON {
			type jh struct {
				Node, Kind, Project, Path, Section, Text string
				Score                                    float64
			}
			out := make([]jh, 0, len(hits))
			for _, h := range hits {
				out = append(out, jh{h.NodeID, h.Kind, h.Project, h.Path, h.Section, h.Text, h.Score})
			}
			json.NewEncoder(os.Stdout).Encode(out)
			return
		}
		if len(hits) == 0 {
			fmt.Println("no results")
		}
		for i, h := range hits {
			sec := ""
			if h.Section != "" {
				sec = "::" + h.Section
			}
			text := strings.Join(strings.Fields(h.Text), " ")
			if len([]rune(text)) > 600 {
				text = string([]rune(text)[:600])
			}
			fmt.Printf("%d. [%s %s%s] score %.3f\n   %s\n", i+1, h.Kind, h.Path, sec, h.Score, text)
		}
	case "embed":
		fs := flag.NewFlagSet("embed", flag.ExitOnError)
		reset := fs.Bool("reset", false, "drop the cached vectors of every model first")
		fs.Parse(rest)
		d := open(p)
		defer d.Close()
		e := embed.New(embed.Default())
		ctx := context.Background()
		hc, cancel := context.WithTimeout(ctx, 60*time.Second)
		dim, err := e.Health(hc)
		cancel()
		if err != nil {
			die("embedder health check failed: %v\nStart the model server and pull the model (for the default: `ollama pull nomic-embed-text`).", err)
		}
		if *reset {
			n, err := d.DropVectors("")
			if err != nil {
				die("%v", err)
			}
			fmt.Printf("dropped %d cached vectors\n", n)
		} else if n, err := d.DropVectors(e.Model()); err == nil && n > 0 {
			fmt.Printf("dropped %d cached vectors of other models or of texts no longer used\n", n)
		}
		missing, total, err := d.Pending(e.Model())
		if err != nil {
			die("%v", err)
		}
		fmt.Printf("model %s, %d dimensions; %d of %d chunk texts need a vector\n", e.Model(), dim, missing, total)
		start := time.Now()
		last := 0
		n, err := d.EmbedPending(ctx, e, func(done, tot int) {
			if done-last >= 200 || done == tot {
				last = done
				rate := float64(done) / time.Since(start).Seconds()
				fmt.Printf("%d/%d  %.1f chunks/s  about %s left\n", done, tot, rate, (time.Duration(float64(tot-done)/rate) * time.Second).Round(time.Minute))
			}
		})
		if err != nil {
			die("embedding stopped after %d chunks (rerun to continue): %v", n, err)
		}
		fmt.Printf("embedded %d chunk texts in %s\n", n, time.Since(start).Round(time.Second))
	case "stats":
		d := open(p)
		defer d.Close()
		m, err := d.Stats()
		if err != nil {
			die("%v", err)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%-40s %d\n", k, m[k])
		}
		if miss, total, err := d.Pending(embed.Default().Model); err == nil {
			fmt.Printf("%-40s %d of %d chunk texts have a vector (%s)\n", "vectors", total-miss, total, embed.Default().Model)
		}
	case "hash":
		d := open(p)
		defer d.Close()
		h, counts, err := d.StateHash()
		if err != nil {
			die("%v", err)
		}
		fmt.Println(h)
		fmt.Fprintf(os.Stderr, "nodes %d  chunks %d  edges %d\n", counts["node"], counts["chunk"], counts["edge"])
	case "digests":
		d := open(p)
		defer d.Close()
		m, err := d.NodeDigests()
		if err != nil {
			die("%v", err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		enc.Encode(m)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func open(p paths) *index.DB {
	d, err := index.Open(p.db)
	if err != nil {
		die("open index %s: %v", p.db, err)
	}
	return d
}

func cmdInit(p paths) {
	for _, d := range []string{"wiki", "notes", "facts", "raw"} {
		if err := os.MkdirAll(filepath.Join(p.repo, d), 0o755); err != nil {
			die("%v", err)
		}
	}
	gi := filepath.Join(p.repo, ".gitignore")
	if _, err := os.Stat(gi); os.IsNotExist(err) {
		os.WriteFile(gi, []byte("*.db\n*.db-shm\n*.db-wal\n"), 0o644)
	}
	pj := filepath.Join(p.repo, "projects.yaml")
	if _, err := os.Stat(pj); os.IsNotExist(err) {
		os.WriteFile(pj, []byte("# projects whose source code is indexed as the code layer\nprojects: []\n"), 0o644)
	}
	if _, err := os.Stat(filepath.Join(p.repo, ".git")); os.IsNotExist(err) {
		if out, err := exec.Command("git", "-C", p.repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "hms: git init failed (%v): %s\n", err, out)
		}
	}
	d := open(p)
	d.Close()
	fmt.Printf("memory repository: %s\nindex:             %s\n", p.repo, p.db)
}
