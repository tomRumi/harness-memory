// Command hms is the command line of harness-memory-service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/curate"
	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/install"
	"github.com/tomRumi/harness-memory-service/internal/server"
	"github.com/tomRumi/harness-memory-service/internal/store"
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
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		yes := fs.Bool("yes", false, "download the embedding model without asking")
		fs.Parse(rest)
		fi, _ := os.Stdin.Stat()
		o := install.InitOptions{Yes: *yes, Interactive: fi != nil && fi.Mode()&os.ModeCharDevice != 0, In: os.Stdin, Out: os.Stdout}
		if err := install.Init(install.Paths{Home: p.home, Repo: p.repo, DB: p.db}, o); err != nil {
			die("init: %v", err)
		}
	case "doctor":
		if install.Doctor(install.Paths{Home: p.home, Repo: p.repo, DB: p.db}, os.Stdout) > 0 {
			os.Exit(1)
		}
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
		kinds := fs.String("k", "", "kinds, comma separated: wiki,note,fact")
		n := fs.Int("n", 5, "number of hits")
		asJSON := fs.Bool("json", false, "print hits as JSON, with the full text")
		withVec := fs.Bool("vectors", install.VectorsEnabled(), "use the vector pass as well as keywords (default on; HMS_VECTORS=0 turns it off)")
		format := fs.String("format", "text", "text, or context for a harness hook (see -hook)")
		hook := fs.Bool("hook", false, "with -format=context: read a UserPromptSubmit payload (JSON with prompt) on standard input")
		fs.Parse(rest)
		if *format == "context" {
			os.Exit(recallContext(p, *proj, *hook, strings.Join(fs.Args(), " "), *withVec, os.Stdin, os.Stdout, os.Stderr))
		}
		d := open(p)
		defer d.Close()
		f := index.Filter{Project: *proj, Limit: *n}
		if *kinds != "" {
			f.Kinds = strings.Split(*kinds, ",")
		}
		query := strings.Join(fs.Args(), " ")
		var hits []index.Hit
		var err error
		header := "" // one line first when the vector pass could not be used
		if !*withVec {
			hits, err = d.Keyword(query, f)
		} else {
			e := embed.New(embed.Default())
			m, merr := d.LoadMatrix(e.ID())
			if merr != nil {
				die("%v", merr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var via string
			var warn error
			hits, via, warn = d.Hybrid(ctx, e, m, query, f)
			if warn == nil {
				warn = index.ProbeIfEmpty(ctx, e, m)
			}
			if warn != nil {
				header = server.VectorsHeader(warn)
			}
			if m.Missing > 0 {
				fmt.Fprintf(os.Stderr, "hms: %d chunks have no vector yet (run `hms embed`); %s result\n", m.Missing, via)
			}
		}
		if err != nil {
			die("%v", err)
		}
		if *asJSON {
			if header != "" {
				fmt.Fprintln(os.Stderr, header)
			}
			type jh struct {
				Node, Kind, Project, Path, Section, Text string
				Score                                    float64
				ScoreKind                                string // "bm25" (lower is better) or "rrf" (higher is better)
			}
			scoreKind := "bm25"
			if *withVec {
				scoreKind = "rrf"
			}
			out := make([]jh, 0, len(hits))
			for _, h := range hits {
				out = append(out, jh{h.NodeID, h.Kind, h.Project, h.Path, h.Section, h.Text, h.Score, scoreKind})
			}
			json.NewEncoder(os.Stdout).Encode(out)
			return
		}
		if header != "" {
			fmt.Println(header)
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
		prune := fs.Bool("prune", false, "also drop cached vectors of other models and of texts no longer used")
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
		} else if *prune {
			n, err := d.DropVectors(e.ID())
			if err != nil {
				die("%v", err)
			}
			fmt.Printf("dropped %d cached vectors of other models or of texts no longer used\n", n)
		}
		missing, total, err := d.Pending(e.ID())
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
	case "bench":
		if len(rest) == 0 {
			die("bench: give at least one questions file")
		}
		var queries []string
		for _, f := range rest {
			raw, err := os.ReadFile(f)
			if err != nil {
				die("%v", err)
			}
			var qs struct {
				Questions []struct{ Question string } `json:"questions"`
			}
			if err := json.Unmarshal(raw, &qs); err != nil {
				die("%s: %v", f, err)
			}
			for _, q := range qs.Questions {
				queries = append(queries, q.Question)
			}
		}
		d := open(p)
		defer d.Close()
		e := embed.New(embed.Default())
		loadStart := time.Now()
		m, err := d.LoadMatrix(e.ID())
		if err != nil {
			die("%v", err)
		}
		loaded := time.Since(loadStart)
		cache := &cachedEmbedder{inner: e, vecs: map[string][]float32{}}
		ctx := context.Background()
		var warm, cold []time.Duration
		for _, q := range queries { // first pass fills the vector cache and measures the whole call, embedding included
			t0 := time.Now()
			if _, _, w := d.Hybrid(ctx, cache, m, q, index.Filter{Limit: 5}); w != nil {
				die("bench: %v", w)
			}
			cold = append(cold, time.Since(t0))
		}
		for round := 0; round < 3; round++ { // then the same calls with the query vector cached: the recall itself
			for _, q := range queries {
				t0 := time.Now()
				d.Hybrid(ctx, cache, m, q, index.Filter{Limit: 5})
				warm = append(warm, time.Since(t0))
			}
		}
		pct := func(v []time.Duration, q float64) time.Duration {
			s := append([]time.Duration(nil), v...)
			sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
			return s[int(float64(len(s)-1)*q)]
		}
		st, _ := d.Stats()
		nodes := 0
		for _, n := range st {
			nodes += n
		}
		fmt.Printf("machine: %s/%s, %d CPUs, %s\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
		distinct := map[string]bool{}
		for _, q := range queries {
			distinct[q] = true
		}
		fmt.Printf("corpus: %d nodes, %d vectors of %d dimensions (wiki, note, fact), %d queries (%d distinct)\n", nodes, len(m.IDs), m.Dim, len(warm), len(distinct))
		fmt.Printf("vector matrix load (once per process): %s\n", loaded.Round(time.Millisecond))
		fmt.Printf("recall without embedding (cached query vector): p50 %s  p95 %s  max %s\n", pct(warm, .5).Round(time.Microsecond), pct(warm, .95).Round(time.Microsecond), pct(warm, 1).Round(time.Microsecond))
		fmt.Printf("recall with embedding of the query:             p50 %s  p95 %s  max %s\n", pct(cold, .5).Round(time.Millisecond), pct(cold, .95).Round(time.Millisecond), pct(cold, 1).Round(time.Millisecond))
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := fs.String("addr", "127.0.0.1:8765", "listen address; must be a loopback address")
		withVec := fs.Bool("vectors", install.VectorsEnabled(), "use the embedder: vector pass in recall, vectors for new notes (default on; HMS_VECTORS=0 turns it off)")
		fs.Parse(rest)
		host, _, err := net.SplitHostPort(*addr)
		if err != nil {
			die("serve: %v", err)
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			die("serve: %q is not a loopback address; the service has no authentication and listens on this machine only", *addr)
		}
		d := open(p)
		defer d.Close()
		var emb *embed.Client
		if *withVec {
			emb = embed.New(embed.Default())
			hc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if _, err := emb.Health(hc); err != nil {
				fmt.Fprintf(os.Stderr, "hms: vectors unavailable (%v): starting with keyword match only; writes still work, and the service embeds what is missing once the embedder answers\n", err)
			}
			cancel()
		}
		com := store.NewCommitter(p.repo, 3*time.Second)
		store.CleanTemp(p.repo)
		com.Sweep() // notes left uncommitted by a crash or an earlier failure
		var e index.Embedder
		if emb != nil {
			e = emb
		}
		st, err := store.New(p.repo, d, e, com, filepath.Join(p.home, "events.db"))
		if err != nil {
			die("serve: %v", err)
		}
		defer st.Close()
		srv, err := server.New(st, d, e)
		if err != nil {
			die("serve: %v", err)
		}
		hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
		catchCtx, stopCatch := context.WithCancel(context.Background())
		catchDone := make(chan struct{})
		go func() { // embeds chunks that have no vector, now and every minute
			defer close(catchDone)
			srv.CatchUp(catchCtx, 60*time.Second)
		}()
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			stopCatch()
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			hs.Shutdown(sctx) // in-flight writes finish first
			cancel()
			com.Flush()
		}()
		fmt.Printf("hms serving MCP on http://%s/mcp (health: /healthz)\n", *addr)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			stopCatch()
			die("serve: %v", err)
		}
		<-stopped // the last commit is made before the process closes the store
		<-catchDone
	case "wiki":
		os.Exit(wikiCmd(p, rest, os.Stdout, os.Stderr))
	case "curate":
		os.Exit(curateCmd(p, rest, os.Stdin, os.Stdout, os.Stderr))
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
		var e index.Embedder
		if install.VectorsEnabled() {
			e = embed.New(embed.Default())
		}
		fmt.Println(server.VectorLine(context.Background(), d, e))
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

// wikiCmd runs `hms wiki links` and `hms wiki index`. It makes the index match the files first, so that it reports
// what is on disk now. It returns the exit status.
func wikiCmd(p paths, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "related" || args[0] == "duplicates" || args[0] == "eval-pairs") {
		return wikiPagesCmd(p, args, stdout, stderr)
	}
	if len(args) == 0 || (args[0] != "links" && args[0] != "index") {
		fmt.Fprintln(stderr, "usage: hms wiki links [-p project] | hms wiki index [-p project] | hms wiki related <page> [-n N] [-p project] | hms wiki duplicates [-p project] [-t threshold] | hms wiki eval-pairs -pairs FILE")
		return 2
	}
	fs := flag.NewFlagSet("wiki "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	proj := fs.String("p", "", "project (default: all projects)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *proj != "" {
		pn, err := store.ValidProject(*proj)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 2
		}
		*proj = pn
	}
	d, err := index.Open(p.db)
	if err != nil {
		fmt.Fprintf(stderr, "hms: open index %s: %v\n", p.db, err)
		return 1
	}
	defer d.Close()
	res, err := d.Rescan(p.repo)
	if err != nil {
		fmt.Fprintf(stderr, "hms: rescan: %v\n", err)
		return 1
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if args[0] == "links" {
		ps, err := d.LinkProblems(*proj)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		broken, ambiguous := 0, 0
		for _, lp := range ps {
			if lp.Status == "broken" {
				broken++
				fmt.Fprintf(stdout, "%s: broken link [[%s]]\n", lp.Page, lp.Link)
			} else {
				ambiguous++
				fmt.Fprintf(stdout, "%s: ambiguous link [[%s]] (could be %s)\n", lp.Page, lp.Link, strings.Join(lp.Matches, ", "))
			}
		}
		fmt.Fprintf(stdout, "links: %d broken, %d ambiguous\n", broken, ambiguous)
		return 0
	}
	projects := []string{*proj}
	if *proj == "" {
		if projects, err = d.Projects(); err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
	}
	com := store.NewCommitter(p.repo, 3*time.Second)
	sweepLeftovers(p.repo, com)
	st, err := store.New(p.repo, d, nil, com, filepath.Join(p.home, "events.db"))
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	defer st.Close() // makes the commit
	status := 0
	for _, pr := range projects {
		rel := "wiki/" + pr + "/Index.md"
		wrote, err := st.WriteIndexPage(context.Background(), pr)
		switch {
		case errors.Is(err, store.ErrHandWritten):
			fmt.Fprintf(stdout, "%s: %v\n", rel, err)
		case err != nil:
			fmt.Fprintf(stderr, "hms: %s: %v\n", rel, err)
			status = 1
		case wrote:
			fmt.Fprintln(stdout, "wrote", rel)
		default:
			fmt.Fprintln(stdout, "unchanged", rel)
		}
	}
	return status
}

// curateCmd runs `hms curate`. It returns the exit status.
func curateCmd(p paths, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("curate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pending := fs.Bool("pending", false, "print the batches to merge as JSON")
	submit := fs.String("submit", "", "read a submission (or a model answer with -p) from FILE, or - for standard input, check it and write it")
	apply := fs.Bool("apply", false, "merge with the built-in model")
	proj := fs.String("p", "", "project (default: all projects; required for a bare answer given to -submit)")
	max := fs.Int("max", curate.DefaultMax, "notes per project per run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	modes := 0
	for _, on := range []bool{*pending, *submit != "", *apply} {
		if on {
			modes++
		}
	}
	if modes > 1 || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: hms curate | hms curate -pending [-p P] [-max N] | hms curate -submit FILE|- [-p P] | hms curate -apply [-p P] [-max N]")
		return 2
	}
	if *proj != "" { // the same rule as the store: lower-case names; no wiki/PROJ is ever made
		pn, err := store.ValidProject(*proj)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 2
		}
		*proj = pn
	}
	if modes == 0 { // read only: files, no index
		ws, err := curate.WaitingNotes(p.repo)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		if len(ws) == 0 {
			fmt.Fprintln(stdout, "no notes waiting")
		}
		for _, w := range ws {
			fmt.Fprintln(stdout, w)
		}
		if n := curate.SkippedNotes(p.repo, *proj); n > 0 {
			fmt.Fprintf(stdout, "%d note files have a name that cannot be a note id (a slash, a backslash or a control character) and are skipped\n", n)
		}
		if ev, err := store.New(p.repo, nil, nil, nil, filepath.Join(p.home, "events.db")); err == nil {
			if ids := ev.RetireCandidates(); len(ids) > 0 {
				names := make([]string, len(ids))
				for i, id := range ids {
					names[i] = filepath.Base(id)
				}
				fmt.Fprintf(stdout, "rated unhelpful by %d sessions, none helpful: %s\n", store.RetireAfter, strings.Join(names, " "))
			}
			ev.Close()
		}
		return 0
	}
	var llm *curate.LLM
	if *apply {
		if llm = curate.LLMFromEnv(); llm == nil {
			fmt.Fprintln(stderr, "no model configured (HMS_LLM_URL, HMS_LLM_MODEL): use -pending and -submit from a harness job")
			return 1
		}
	}
	var text string
	if *submit != "" {
		var raw []byte
		var err error
		if *submit == "-" {
			raw, err = io.ReadAll(io.LimitReader(stdin, 32<<20))
		} else {
			raw, err = os.ReadFile(*submit)
		}
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		text = string(raw)
	}
	d, err := index.Open(p.db)
	if err != nil {
		fmt.Fprintf(stderr, "hms: open index %s: %v\n", p.db, err)
		return 1
	}
	defer d.Close()
	res, err := d.Rescan(p.repo) // the index must match the files: related pages and links are read from it
	if err != nil {
		fmt.Fprintf(stderr, "hms: rescan: %v\n", err)
		return 1
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	var e index.Embedder
	if install.VectorsEnabled() {
		e = embed.New(embed.Default())
	}
	com := store.NewCommitter(p.repo, 3*time.Second)
	if *submit != "" || *apply {
		sweepLeftovers(p.repo, com)
	}
	st, err := store.New(p.repo, d, e, com, filepath.Join(p.home, "events.db"))
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	defer st.Close() // makes the commit
	ctx := context.Background()
	switch {
	case *pending:
		bs, err := curate.Pending(ctx, st, *proj, *max)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		if bs == nil {
			bs = []curate.Batch{}
		}
		out, _ := json.MarshalIndent(bs, "", "  ")
		fmt.Fprintln(stdout, string(out))
	case *submit != "":
		sub, err := curate.ParseSubmission(text, *proj)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		rep, err := curate.Submit(ctx, st, sub)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, rep)
	default: // apply
		results, err := curate.Apply(ctx, st, llm, *proj, *max)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		if len(results) == 0 {
			fmt.Fprintln(stdout, "no notes waiting")
		}
		status := 0
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(stderr, "hms: %s: notes left pending: %v\n", r.Project, r.Err)
				status = 1
				continue
			}
			fmt.Fprint(stdout, r.Report)
		}
		return status
	}
	return 0
}

func open(p paths) *index.DB {
	d, err := index.Open(p.db)
	if err != nil {
		die("open index %s: %v", p.db, err)
	}
	return d
}

// cachedEmbedder remembers query vectors so that a benchmark can time the recall without the model call.
type cachedEmbedder struct {
	inner *embed.Client
	vecs  map[string][]float32
}

func (c *cachedEmbedder) Model() string { return c.inner.Model() }
func (c *cachedEmbedder) ID() string    { return c.inner.ID() }
func (c *cachedEmbedder) Docs(ctx context.Context, t []string) ([][]float32, error) {
	return c.inner.Docs(ctx, t)
}
func (c *cachedEmbedder) Query(ctx context.Context, t string) ([]float32, error) {
	if v, ok := c.vecs[t]; ok {
		return v, nil
	}
	v, err := c.inner.Query(ctx, t)
	if err == nil {
		c.vecs[t] = v
	}
	return v, err
}

// ContextTimeout bounds the whole hook run. After it the hook prints nothing and the prompt goes on unchanged.
const ContextTimeout = 3 * time.Second

// recallContext prints the memory block for a hook. It always returns 0: a hook must never block or fail a prompt.
// Problems go to standard error.
func recallContext(p paths, proj string, hook bool, query string, vectors bool, stdin io.Reader, stdout, stderr io.Writer) int {
	if hook {
		var in struct {
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in); err != nil {
			fmt.Fprintln(stderr, "hms: hook payload not read:", err)
			return 0
		}
		query = in.Prompt
	}
	if r := []rune(strings.TrimSpace(query)); len(r) > 400 {
		query = string(r[:400])
	}
	if strings.TrimSpace(query) == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), ContextTimeout)
	defer cancel()
	type result struct {
		block string
		warn  error
	}
	done := make(chan result, 1)
	go func() {
		d, err := index.OpenReadOnly(p.db)
		if err != nil {
			fmt.Fprintln(stderr, "hms: memory unavailable:", err)
			done <- result{}
			return
		}
		defer d.Close()
		var e index.Embedder
		if vectors {
			e = embed.New(embed.Default())
		}
		srv, err := server.NewReader(d, e)
		if err != nil {
			fmt.Fprintln(stderr, "hms: memory unavailable:", err)
			done <- result{}
			return
		}
		b, w := srv.ContextBlock(ctx, query, proj, 2500)
		done <- result{b, w}
	}()
	select {
	case r := <-done:
		if r.warn != nil { // the warning never goes into the prompt block
			fmt.Fprintln(stderr, "hms:", server.VectorsHeader(r.warn))
		}
		if r.block != "" {
			fmt.Fprintln(stdout, r.block)
		}
	case <-ctx.Done():
		fmt.Fprintln(stderr, "hms: recall took longer than", ContextTimeout, "- skipped")
	}
	return 0
}

// sweepLeftovers commits the notes and pages that an earlier run left uncommitted (it was killed between writing and
// committing). Every command that writes does it first, so the files never wait for a serve to be started.
func sweepLeftovers(repo string, com *store.Committer) {
	store.CleanTemp(repo)
	com.Sweep()
	com.Flush()
}
