package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/tomRumi/harness-memory/internal/index"
	"github.com/tomRumi/harness-memory/internal/store"
)

const wikiUsage = "usage: hms wiki links [-p project] | hms wiki index [-p project] | hms wiki related <page> [-n N] [-p project] | hms wiki duplicates [-p project] [-t threshold] | hms wiki eval-pairs -pairs FILE"

// wikiCmd runs `hms wiki links` and `hms wiki index`. It makes the index match the files first, so that it reports
// what is on disk now. It returns the exit status.
func wikiCmd(p paths, args []string, stdout, stderr io.Writer) (code int) {
	if len(args) > 0 && (args[0] == "related" || args[0] == "duplicates" || args[0] == "eval-pairs") {
		return wikiPagesCmd(p, args, stdout, stderr)
	}
	if len(args) == 0 || (args[0] != "links" && args[0] != "index") {
		fmt.Fprintln(stderr, wikiUsage)
		return 2
	}
	fs := flag.NewFlagSet("wiki "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	proj := fs.String("p", "", "project (default: all projects)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if !projectArg(proj, stderr) {
		return 2
	}
	d, code := openRescanned(p, stderr)
	if d == nil {
		return code
	}
	defer d.Close()
	if args[0] == "links" {
		return wikiLinks(d, *proj, stdout, stderr)
	}
	return wikiIndex(p, d, *proj, stdout, stderr)
}

// wikiLinks lists the broken and ambiguous links, one line each, then a summary.
func wikiLinks(d *index.DB, proj string, stdout, stderr io.Writer) int {
	ps, err := d.LinkProblems(proj)
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

// wikiIndex writes wiki/<project>/Index.md of the project (every project when empty) when its content changed.
func wikiIndex(p paths, d *index.DB, proj string, stdout, stderr io.Writer) (code int) {
	projects := []string{proj}
	if proj == "" {
		var err error
		if projects, err = d.Projects(); err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
	}
	st, err := openStore(p, d, nil, true, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	defer closeStore(st, stderr, &code) // makes the commit and says so when it fails
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
