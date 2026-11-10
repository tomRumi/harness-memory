package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory/internal/embed"
	"github.com/tomRumi/harness-memory/internal/index"
	"github.com/tomRumi/harness-memory/internal/install"
	"github.com/tomRumi/harness-memory/internal/server"
	"github.com/tomRumi/harness-memory/internal/wiki"
)

func cmdRecall(p paths, rest []string) {
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	proj := fs.String("p", "", "project")
	kinds := fs.String("k", "", "kinds, comma separated: wiki,note,fact")
	n := fs.Int("n", 5, "number of hits")
	asJSON := fs.Bool("json", false, "print hits as JSON, with the full text")
	withVec := fs.Bool("vectors", install.VectorsEnabled(), "use the vector pass as well as keywords (default on; HMS_VECTORS=0 turns it off)")
	format := fs.String("format", "text", "text, or context for a harness hook (see -hook)")
	hook := fs.Bool("hook", false, "with -format=context: read a UserPromptSubmit payload (JSON with prompt) on standard input")
	fs.Parse(rest)
	if !projectArg(proj, os.Stderr) {
		os.Exit(2)
	}
	query := strings.Join(fs.Args(), " ")
	if *format == "context" {
		os.Exit(recallContext(p, *proj, *hook, query, *withVec, os.Stdin, os.Stdout, os.Stderr))
	}
	d := open(p)
	defer d.Close()
	f := index.Filter{Project: *proj, Limit: *n}
	if *kinds != "" {
		f.Kinds = strings.Split(*kinds, ",")
	}
	hits, header := recallHits(d, query, f, *withVec)
	if *asJSON {
		if header != "" {
			fmt.Fprintln(os.Stderr, header)
		}
		printHitsJSON(hits, *withVec)
		return
	}
	if header != "" {
		fmt.Println(header)
	}
	printHits(hits)
}

// recallHits runs the search. header is one line to print first when the vector pass could not be used.
func recallHits(d *index.DB, query string, f index.Filter, withVec bool) (hits []index.Hit, header string) {
	if !withVec {
		hits, err := d.Keyword(query, f)
		if err != nil {
			die("%v", err)
		}
		return hits, ""
	}
	e := embed.New(embed.Default())
	m, err := d.LoadMatrix(e.ID())
	if err != nil {
		die("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hits, via, warn := d.Hybrid(ctx, e, m, query, f)
	if warn == nil {
		warn = index.ProbeIfEmpty(ctx, e, m)
	}
	if warn != nil {
		header = server.VectorsHeader(warn)
	}
	if m.Missing > 0 {
		fmt.Fprintf(os.Stderr, "hms: %d chunks have no vector yet (run `hms embed`); %s result\n", m.Missing, via)
	}
	return hits, header
}

func printHitsJSON(hits []index.Hit, withVec bool) {
	type jh struct {
		Node, Kind, Project, Path, Section, Text string
		Score                                    float64
		ScoreKind                                string // "bm25" (lower is better) or "rrf" (higher is better)
	}
	scoreKind := "bm25"
	if withVec {
		scoreKind = "rrf"
	}
	out := make([]jh, 0, len(hits))
	for _, h := range hits {
		out = append(out, jh{h.NodeID, h.Kind, h.Project, h.Path, h.Section, h.Text, h.Score, scoreKind})
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

func printHits(hits []index.Hit) {
	if len(hits) == 0 {
		fmt.Println("no results")
	}
	for i, h := range hits {
		sec := ""
		if h.Section != "" {
			sec = "::" + h.Section
		}
		text := wiki.Cut(strings.Join(strings.Fields(h.Text), " "), 600)
		fmt.Printf("%d. [%s %s%s] score %.3f\n   %s\n", i+1, h.Kind, h.Path, sec, h.Score, text)
	}
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
