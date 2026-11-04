// Command hms is the command line of harness-memory-service.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/server"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

const usage = `hms: markdown-backed memory with a rebuildable index

usage: hms [--home DIR] [--repo DIR] [--db FILE] <command> [arguments]

  init                         create the memory repository and the index
  rescan                       make the index match the files (changed files only)
  embed [-reset] [-prune]      embed every wiki, note and fact chunk that has no vector yet; safe to stop and rerun
  recall [-p P] [-k K,K] [-n N] [-vectors] [-json] <words...>
                               keyword recall over active nodes; -vectors (or HMS_VECTORS=1) adds the vector pass
  bench <questions.json> ...   recall latency over the questions' texts (query vector cached, then with embedding)
  serve [-addr A] [-vectors]  MCP over HTTP (loopback only) with seven tools: recall learn wiki_read fact_add retire feedback stats
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
		withVec := fs.Bool("vectors", os.Getenv("HMS_VECTORS") == "1", "add the vector pass (needs `hms embed` and a running model server); also HMS_VECTORS=1")
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
		withVec := fs.Bool("vectors", os.Getenv("HMS_VECTORS") == "1", "use the embedder: vector pass in recall, vectors for new notes (default off: keyword match only)")
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
			hc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if _, err := emb.Health(hc); err != nil {
				fmt.Fprintf(os.Stderr, "hms: embedder not reachable (%v): starting with keyword match only; writes will be refused until it is back\n", err)
			}
			cancel()
		}
		com := store.NewCommitter(p.repo, 3*time.Second)
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
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			com.Flush()
			hs.Shutdown(context.Background())
		}()
		fmt.Printf("hms serving MCP on http://%s/mcp (health: /healthz)\n", *addr)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			die("serve: %v", err)
		}
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
		if miss, total, err := d.Pending(embed.New(embed.Default()).ID()); err == nil {
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
