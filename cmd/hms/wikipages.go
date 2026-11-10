package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tomRumi/harness-memory/internal/embed"
	"github.com/tomRumi/harness-memory/internal/index"
)

// pagesFlags are the arguments of `hms wiki related`, `duplicates` and `eval-pairs`.
type pagesFlags struct {
	sub, page, proj, pairs string
	n                      int
	thr                    float64
}

// wikiPagesCmd runs `hms wiki related`, `hms wiki duplicates` and `hms wiki eval-pairs`. They read the page vectors
// that `hms embed` stored; they never call the embedder. The index is made to match the files first.
func wikiPagesCmd(p paths, args []string, stdout, stderr io.Writer) int {
	f, code := parsePagesArgs(args, stderr)
	if code != 0 {
		return code
	}
	var spec pairSpec
	if f.sub == "eval-pairs" {
		raw, err := os.ReadFile(f.pairs)
		if err == nil {
			err = json.Unmarshal(raw, &spec)
		}
		if err != nil {
			fmt.Fprintf(stderr, "hms: pairs file: %v\n", err)
			return 1
		}
	}
	d, code := openRescanned(p, stderr)
	if d == nil {
		return code
	}
	defer d.Close()
	m, err := d.LoadMatrix(embed.New(embed.Default()).ID())
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	switch f.sub {
	case "related":
		return wikiRelated(d, m, f, stdout, stderr)
	case "duplicates":
		return wikiDuplicates(d, m, f, stdout, stderr)
	}
	evalPairs(stdout, m, spec)
	return 0
}

// parsePagesArgs reads the flags (they may come before or after the page) and checks the usage. A non-zero status
// means the command ends with it.
func parsePagesArgs(args []string, stderr io.Writer) (f pagesFlags, code int) {
	f.sub = args[0]
	fs := flag.NewFlagSet("wiki "+f.sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.proj, "p", "", "project")
	fs.IntVar(&f.n, "n", 5, "number of related pages")
	fs.Float64Var(&f.thr, "t", index.DuplicateThreshold, "duplicate threshold (cosine)")
	fs.StringVar(&f.pairs, "pairs", "", "pairs file")
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return f, 2
		}
		if fs.NArg() == 0 {
			break
		}
		if f.page != "" || f.sub != "related" {
			fmt.Fprintf(stderr, "hms: unexpected argument %q\n", fs.Arg(0))
			return f, 2
		}
		f.page, rest = fs.Arg(0), fs.Args()[1:]
	}
	if !projectArg(&f.proj, stderr) {
		return f, 2
	}
	switch {
	case f.sub == "related" && f.page == "":
		fmt.Fprintln(stderr, "usage: hms wiki related <title or path> [-n 5] [-p project]")
		return f, 2
	case f.sub == "eval-pairs" && f.pairs == "":
		fmt.Fprintln(stderr, "usage: hms wiki eval-pairs -pairs FILE")
		return f, 2
	}
	return f, 0
}

// pageName is [[Title]], with the project in front when it is not home.
func pageName(id, home string) string {
	proj, title, _ := strings.Cut(strings.TrimPrefix(id, "wiki/"), "/")
	title = title[strings.LastIndex(title, "/")+1:]
	if proj == home {
		return "[[" + title + "]]"
	}
	return "[[" + proj + "/" + title + "]]"
}

func wikiRelated(d *index.DB, m *index.Matrix, f pagesFlags, stdout, stderr io.Writer) int {
	q := f.page
	if f.proj != "" && !strings.Contains(q, "/") {
		q = f.proj + "/" + q
	}
	id, err := d.ResolvePage(q)
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	home, _, _ := strings.Cut(strings.TrimPrefix(id, "wiki/"), "/")
	rel, ok := m.RelatedPages(id, f.n)
	if !ok {
		fmt.Fprintf(stdout, "no vectors yet for %s (run `hms embed`)\n", pageName(id, ""))
		return 1
	}
	for _, r := range rel {
		fmt.Fprintln(stdout, strings.TrimRight(fmt.Sprintf("%.2f  %s  %s", r.Score, pageName(r.NodeID, home), oneLineSummary(d, r.NodeID)), " "))
	}
	if len(rel) == 0 {
		fmt.Fprintln(stdout, "no other pages with vectors")
	}
	return 0
}

func wikiDuplicates(d *index.DB, m *index.Matrix, f pagesFlags, stdout, stderr io.Writer) int {
	pages, err := d.WikiPages(f.proj)
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	missing := 0
	for _, pg := range pages {
		if _, ok := m.PageVector(pg.NodeID); !ok {
			missing++
		}
	}
	prs := m.NearDuplicates(f.proj, float32(f.thr))
	for _, pr := range prs {
		fmt.Fprintf(stdout, "%.2f  %s  %s\n", pr.Score, pageName(pr.A.NodeID, f.proj), pageName(pr.B.NodeID, f.proj))
	}
	fmt.Fprintf(stdout, "duplicates: %d (cosine >= %.2f; %d pages compared, %d with no vectors yet)\n", len(prs), f.thr, len(pages)-missing, missing)
	return 0
}

func oneLineSummary(d *index.DB, id string) string {
	var s string
	d.QueryRow(`SELECT summary FROM nodes WHERE id=?`, id).Scan(&s)
	return strings.Join(strings.Fields(s), " ")
}

// pairSpec is the frozen pairs file. The copies of near_duplicates.sources are not in it: each is expected in
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

// evalTally counts the passing pairs of the three kinds (related, unrelated, duplicate) and the extremes.
type evalTally struct {
	pass, total              [3]int
	minRel, maxUnrel         float32
	minRelPair, maxUnrelPair string
}

// evalPairs prints the scores of the three kinds of pair. It changes nothing.
func evalPairs(w io.Writer, m *index.Matrix, spec pairSpec) {
	t := evalTally{minRel: 2, maxUnrel: -2}
	fmt.Fprintf(w, "duplicate threshold %.2f, top %d, %d vectors\n", index.DuplicateThreshold, evalTop, len(m.IDs))
	fmt.Fprintln(w, "kind       result  cosine  rank a>b  rank b>a  pair")
	t.listed(w, m, spec)
	others := t.copies(w, m, spec)
	fmt.Fprintf(w, "related pairs passing: %d/%d\n", t.pass[0], t.total[0])
	fmt.Fprintf(w, "unrelated pairs passing: %d/%d\n", t.pass[1], t.total[1])
	fmt.Fprintf(w, "duplicate copies found: %d/%d\n", t.pass[2], t.total[2])
	if t.total[0] > 0 && t.minRel <= 1 {
		fmt.Fprintf(w, "smallest related cosine: %.3f (%s)\n", t.minRel, t.minRelPair)
	} else {
		fmt.Fprintln(w, "smallest related cosine: none")
	}
	if t.total[1] > 0 && t.maxUnrel >= -1 {
		fmt.Fprintf(w, "largest unrelated cosine: %.3f (%s)\n", t.maxUnrel, t.maxUnrelPair)
	} else {
		fmt.Fprintln(w, "largest unrelated cosine: none")
	}
	fmt.Fprintf(w, "non-copy pairs flagged as duplicates at %.2f: %d\n", index.DuplicateThreshold, len(others))
	for _, o := range others {
		fmt.Fprintln(w, o)
	}
}

// rank is the 1-based rank of other in the related list of of; 0: not listed or no vector.
func rank(m *index.Matrix, of, other string) int {
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

func rankText(r int) string {
	if r == 0 {
		return "-"
	}
	return fmt.Sprint(r)
}

// listed scores the related and the unrelated pairs of the file.
func (t *evalTally) listed(w io.Writer, m *index.Matrix, spec pairSpec) {
	for kind, list := range [][][]string{spec.Related, spec.Unrelated} {
		label := []string{"related", "unrelated"}[kind]
		for _, pr := range list {
			if len(pr) != 2 {
				continue
			}
			a, b := pageID(pr[0]), pageID(pr[1])
			t.total[kind]++
			cos, ok := m.Cosine(a, b)
			ra, rb := rank(m, a, b), rank(m, b, a)
			inA, inB := ra > 0 && ra <= evalTop, rb > 0 && rb <= evalTop
			okPair := ok && inA && inB
			if kind == 1 {
				okPair = ok && !inA && !inB
			}
			res := "FAIL"
			if okPair {
				res = "PASS"
				t.pass[kind]++
			}
			score := "no vectors yet"
			if ok {
				score = fmt.Sprintf("%.3f", cos)
				if kind == 0 && cos < t.minRel {
					t.minRel, t.minRelPair = cos, pr[0]+" <-> "+pr[1]
				}
				if kind == 1 && cos > t.maxUnrel {
					t.maxUnrel, t.maxUnrelPair = cos, pr[0]+" <-> "+pr[1]
				}
			}
			fmt.Fprintf(w, "%-10s %-6s  %-6s  %-8s  %-8s  %s <-> %s\n", label, res, score, rankText(ra), rankText(rb), pr[0], pr[1])
		}
	}
}

// copies checks that each expected copy is flagged as a duplicate. It returns the lines for the other flagged pairs.
func (t *evalTally) copies(w io.Writer, m *index.Matrix, spec pairSpec) (others []string) {
	flagged := m.NearDuplicates("", index.DuplicateThreshold)
	isCopy := map[string]bool{}
	for _, src := range spec.NearDuplicates.Sources {
		a := pageID(src)
		b := a + " (copy)"
		isCopy[a+"|"+b] = true
		t.total[2]++
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
			t.pass[2]++
		}
		if ok {
			score = fmt.Sprintf("%.3f", cos)
		}
		fmt.Fprintf(w, "%-10s %-6s  %-6s  %-8s  %-8s  %s <-> %s (copy).md\n", "duplicate", res, score, "", "", src, strings.TrimSuffix(src, ".md"))
	}
	for _, f := range flagged {
		a, b := f.A.NodeID, f.B.NodeID
		if !isCopy[a+"|"+b] && !isCopy[b+"|"+a] {
			others = append(others, fmt.Sprintf("  %.3f  %s <-> %s", f.Score, a, b))
		}
	}
	return others
}
