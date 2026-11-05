package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/install"
	"github.com/tomRumi/harness-memory-service/internal/server"
)

func cmdInit(p paths, rest []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	yes := fs.Bool("yes", false, "download the embedding model without asking")
	fs.Parse(rest)
	fi, _ := os.Stdin.Stat()
	o := install.InitOptions{Yes: *yes, Interactive: fi != nil && fi.Mode()&os.ModeCharDevice != 0, In: os.Stdin, Out: os.Stdout}
	if err := install.Init(install.Paths{Home: p.home, Repo: p.repo, DB: p.db}, o); err != nil {
		die("init: %v", err)
	}
}

func cmdDoctor(p paths, rest []string) {
	if install.Doctor(install.Paths{Home: p.home, Repo: p.repo, DB: p.db}, os.Stdout) > 0 {
		os.Exit(1)
	}
}

func cmdRescan(p paths, rest []string) {
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
}

func cmdStats(p paths, rest []string) {
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
}

func cmdHash(p paths, rest []string) {
	d := open(p)
	defer d.Close()
	h, counts, err := d.StateHash()
	if err != nil {
		die("%v", err)
	}
	fmt.Println(h)
	fmt.Fprintf(os.Stderr, "nodes %d  chunks %d  edges %d\n", counts["node"], counts["chunk"], counts["edge"])
}

func cmdDigests(p paths, rest []string) {
	d := open(p)
	defer d.Close()
	m, err := d.NodeDigests()
	if err != nil {
		die("%v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	enc.Encode(m)
}
