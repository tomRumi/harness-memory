package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tomRumi/harness-memory-service/internal/curate"
	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/install"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

const curateUsage = "usage: hms curate | hms curate -pending [-p P] [-max N] | hms curate -submit FILE|- [-p P] | hms curate -apply [-p P] [-max N]"

// curateFlags are the flags of `hms curate`.
type curateFlags struct {
	pending, apply bool
	submit, proj   string
	max            int
}

// modes is how many of -pending, -submit and -apply are set.
func (f curateFlags) modes() (n int) {
	for _, on := range []bool{f.pending, f.submit != "", f.apply} {
		if on {
			n++
		}
	}
	return n
}

// curateCmd runs `hms curate`. It returns the exit status.
func curateCmd(p paths, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("curate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f curateFlags
	fs.BoolVar(&f.pending, "pending", false, "print the batches to merge as JSON")
	fs.StringVar(&f.submit, "submit", "", "read a submission (or a model answer with -p) from FILE, or - for standard input, check it and write it")
	fs.BoolVar(&f.apply, "apply", false, "merge with the built-in model")
	fs.StringVar(&f.proj, "p", "", "project (default: all projects; required for a bare answer given to -submit)")
	fs.IntVar(&f.max, "max", curate.DefaultMax, "notes per project per run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if f.modes() > 1 || fs.NArg() > 0 {
		fmt.Fprintln(stderr, curateUsage)
		return 2
	}
	if !projectArg(&f.proj, stderr) {
		return 2
	}
	if f.modes() == 0 { // read only: files, no index
		return curateList(p, f.proj, stdout, stderr)
	}
	var llm *curate.LLM
	if f.apply {
		if llm = curate.LLMFromEnv(); llm == nil {
			fmt.Fprintln(stderr, "no model configured (HMS_LLM_URL, HMS_LLM_MODEL): use -pending and -submit from a harness job")
			return 1
		}
	}
	text, err := readSubmission(f.submit, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	d, code := openRescanned(p, stderr) // the index must match the files: related pages and links are read from it
	if d == nil {
		return code
	}
	defer d.Close()
	var e index.Embedder
	if install.VectorsEnabled() {
		e = embed.New(embed.Default())
	}
	st, err := openStore(p, d, e, f.submit != "" || f.apply, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "hms: %v\n", err)
		return 1
	}
	defer closeStore(st, stderr, &code) // makes the commit and says so when it fails
	return curateRun(st, f, llm, text, stdout, stderr)
}

// readSubmission reads the file (or standard input for "-") that -submit names; "" is no submission.
func readSubmission(name string, stdin io.Reader) (string, error) {
	var raw []byte
	var err error
	switch name {
	case "":
		return "", nil
	case "-":
		raw, err = io.ReadAll(io.LimitReader(stdin, 32<<20))
	default:
		raw, err = os.ReadFile(name)
	}
	return string(raw), err
}

// curateList prints the notes waiting per project and the notes rated unhelpful (read only).
func curateList(p paths, proj string, stdout, stderr io.Writer) int {
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
	if n := curate.SkippedNotes(p.repo, proj); n > 0 {
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

// curateRun does the one mode that was asked for.
func curateRun(st *store.Store, f curateFlags, llm *curate.LLM, text string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	switch {
	case f.pending:
		bs, err := curate.Pending(ctx, st, f.proj, f.max)
		if err != nil {
			fmt.Fprintf(stderr, "hms: %v\n", err)
			return 1
		}
		if bs == nil {
			bs = []curate.Batch{}
		}
		out, _ := json.MarshalIndent(bs, "", "  ")
		fmt.Fprintln(stdout, string(out))
	case f.submit != "":
		sub, err := curate.ParseSubmission(text, f.proj)
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
		return curateApply(ctx, st, llm, f, stdout, stderr)
	}
	return 0
}

func curateApply(ctx context.Context, st *store.Store, llm *curate.LLM, f curateFlags, stdout, stderr io.Writer) int {
	results, err := curate.Apply(ctx, st, llm, f.proj, f.max)
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
