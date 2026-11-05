package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
)

// wikiPagesCmd runs `hms wiki related`, `hms wiki duplicates` and `hms wiki eval-pairs`. They read the page vectors
// that `hms embed` stored; they never call the embedder. The index is made to match the files first.
func wikiPagesCmd(p paths, args []string, stdout, stderr io.Writer) int {
	sub := args[0]
	fs := flag.NewFlagSet("wiki "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	proj := fs.String("p", "", "project")
	n := fs.Int("n", 5, "number of related pages")
	thr := fs.Float64("t", index.DuplicateThreshold, "duplicate threshold (cosine)")
	pairs := fs.String("pairs", "", "pairs file")
	var page string
	rest := args[1:]
	for { // flags may come before or after the page
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		if page != "" || sub != "related" {
			fmt.Fprintf(stderr, "hms: unexpected argument %q\n", fs.Arg(0))
			return 2
		}
		page, rest = fs.Arg(0), fs.Args()[1:]
	}
	if !projectArg(proj, stderr) {
		return 2
	}
	switch {
	case sub == "related" && page == "":
		fmt.Fprintln(stderr, "usage: hms wiki related <title or path> [-n 5] [-p project]")
		return 2
	case sub == "eval-pairs" && *pairs == "":
		fmt.Fprintln(stderr, "usage: hms wiki eval-pairs -pairs FILE")
		return 2
	}
	var spec pairSpec
	if sub == "eval-pairs" {
		raw, err := os.ReadFile(*pairs)
		if err == nil {
			err = json.Unmarshal(raw, &spec)
		}
		if err != nil {
			fmt.Fprintf(stderr, "hms: pairs file: %v\n", err)
			return 1
		}
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
	m, err := d.LoadMatrix(embed.New(embed.Default()).ID())
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	pages, err := d.WikiPages(*proj)
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	byID := map[string]index.PageInfo{}
	missing := 0
	for _, pg := range pages {
		byID[pg.NodeID] = pg
		if _, ok := m.PageVector(pg.NodeID); !ok {
			missing++
		}
	}
	name := func(id, home string) string { // [[Title]], with the project in front when it is not home
		proj, title, _ := strings.Cut(strings.TrimPrefix(id, "wiki/"), "/")
		if proj == home {
			return "[[" + title[strings.LastIndex(title, "/")+1:] + "]]"
		}
		return "[[" + proj + "/" + title[strings.LastIndex(title, "/")+1:] + "]]"
	}
	switch sub {
	case "related":
		q := page
		if *proj != "" && !strings.Contains(q, "/") {
			q = *proj + "/" + q
		}
		id, err := d.ResolvePage(q)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		home, _, _ := strings.Cut(strings.TrimPrefix(id, "wiki/"), "/")
		rel, ok := m.RelatedPages(id, *n)
		if !ok {
			fmt.Fprintf(stdout, "no vectors yet for %s (run `hms embed`)\n", name(id, ""))
			return 1
		}
		for _, r := range rel {
			fmt.Fprintln(stdout, strings.TrimRight(fmt.Sprintf("%.2f  %s  %s", r.Score, name(r.NodeID, home), oneLineSummary(d, r.NodeID)), " "))
		}
		if len(rel) == 0 {
			fmt.Fprintln(stdout, "no other pages with vectors")
		}
	case "duplicates":
		prs := m.NearDuplicates(*proj, float32(*thr))
		home := *proj
		for _, pr := range prs {
			fmt.Fprintf(stdout, "%.2f  %s  %s\n", pr.Score, name(pr.A.NodeID, home), name(pr.B.NodeID, home))
		}
		fmt.Fprintf(stdout, "duplicates: %d (cosine >= %.2f; %d pages compared, %d with no vectors yet)\n", len(prs), *thr, len(pages)-missing, missing)
	case "eval-pairs":
		evalPairs(stdout, m, spec)
	}
	return 0
}

func oneLineSummary(d *index.DB, id string) string {
	var s string
	d.QueryRow(`SELECT summary FROM nodes WHERE id=?`, id).Scan(&s)
	return strings.Join(strings.Fields(s), " ")
}

// pairSpec is the owner-frozen pairs file. The copies of near_duplicates.sources are not in it: each is expected in
// the wiki as "<source without .md> (copy).md".
type pairSpec struct {
	Related        [][]string `json:"related"`
	Unrelated      [][]string `json:"unrelated"`
	NearDuplicates struct {
		Sources []string `json:"sources"`
	} `json:"near_duplicates"`
}

func pageID(path string) string {
	return "wiki/" + strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(path, "wiki/"), "/"), ".md")
}

const evalTop = 5

// evalPairs prints the scores of the three kinds of pair. It changes nothing.
func evalPairs(w io.Writer, m *index.Matrix, spec pairSpec) {
	rank := func(of, other string) int { // 1-based rank of other in the related list of of; 0: not listed or no vector
		rel, ok := m.RelatedPages(of, -1)
		if !ok {
			return 0
		}
		for i, r := range rel {
			if r.NodeID == other {
				return i + 1
			}
		}
		return 0
	}
	fmt.Fprintf(w, "duplicate threshold %.2f, top %d, %d vectors\n", index.DuplicateThreshold, evalTop, len(m.IDs))
	fmt.Fprintln(w, "kind       result  cosine  rank a>b  rank b>a  pair")
	rk := func(r int) string {
		if r == 0 {
			return "-"
		}
		return fmt.Sprint(r)
	}
	var pass [3]int
	var total [3]int
	minRel, maxUnrel := float32(2), float32(-2)
	var minRelPair, maxUnrelPair string
	for kind, list := range [][][]string{spec.Related, spec.Unrelated} {
		label := []string{"related", "unrelated"}[kind]
		for _, pr := range list {
			if len(pr) != 2 {
				continue
			}
			a, b := pageID(pr[0]), pageID(pr[1])
			total[kind]++
			cos, ok := m.Cosine(a, b)
			ra, rb := rank(a, b), rank(b, a)
			inA, inB := ra > 0 && ra <= evalTop, rb > 0 && rb <= evalTop
			var okPair bool
			if kind == 0 {
				okPair = ok && inA && inB
			} else {
				okPair = ok && !inA && !inB
			}
			res := "FAIL"
			if okPair {
				res = "PASS"
				pass[kind]++
			}
			score := "no vectors yet"
			if ok {
				score = fmt.Sprintf("%.3f", cos)
				if kind == 0 && cos < minRel {
					minRel, minRelPair = cos, pr[0]+" <-> "+pr[1]
				}
				if kind == 1 && cos > maxUnrel {
					maxUnrel, maxUnrelPair = cos, pr[0]+" <-> "+pr[1]
				}
			}
			fmt.Fprintf(w, "%-10s %-6s  %-6s  %-8s  %-8s  %s <-> %s\n", label, res, score, rk(ra), rk(rb), pr[0], pr[1])
		}
	}
	flagged := m.NearDuplicates("", index.DuplicateThreshold)
	isCopy := map[string]bool{}
	for _, src := range spec.NearDuplicates.Sources {
		a := pageID(src)
		b := a + " (copy)"
		isCopy[a+"|"+b] = true
		total[2]++
		cos, ok := m.Cosine(a, b)
		found := false
		for _, f := range flagged {
			if (f.A.NodeID == a && f.B.NodeID == b) || (f.A.NodeID == b && f.B.NodeID == a) {
				found = true
			}
		}
		res, score := "FAIL", "no vectors yet"
		if found {
			res = "PASS"
			pass[2]++
		}
		if ok {
			score = fmt.Sprintf("%.3f", cos)
		}
		fmt.Fprintf(w, "%-10s %-6s  %-6s  %-8s  %-8s  %s <-> %s (copy).md\n", "duplicate", res, score, "", "", src, strings.TrimSuffix(src, ".md"))
	}
	var others []string
	for _, f := range flagged {
		a, b := f.A.NodeID, f.B.NodeID
		if !isCopy[a+"|"+b] && !isCopy[b+"|"+a] {
			others = append(others, fmt.Sprintf("  %.3f  %s <-> %s", f.Score, a, b))
		}
	}
	fmt.Fprintf(w, "related pairs passing: %d/%d\n", pass[0], total[0])
	fmt.Fprintf(w, "unrelated pairs passing: %d/%d\n", pass[1], total[1])
	fmt.Fprintf(w, "duplicate copies found: %d/%d\n", pass[2], total[2])
	if total[0] > 0 && minRel <= 1 {
		fmt.Fprintf(w, "smallest related cosine: %.3f (%s)\n", minRel, minRelPair)
	} else {
		fmt.Fprintln(w, "smallest related cosine: none")
	}
	if total[1] > 0 && maxUnrel >= -1 {
		fmt.Fprintf(w, "largest unrelated cosine: %.3f (%s)\n", maxUnrel, maxUnrelPair)
	} else {
		fmt.Fprintln(w, "largest unrelated cosine: none")
	}
	fmt.Fprintf(w, "non-copy pairs flagged as duplicates at %.2f: %d\n", index.DuplicateThreshold, len(others))
	for _, o := range others {
		fmt.Fprintln(w, o)
	}
}
