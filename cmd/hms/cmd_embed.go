package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/embed"
)

func cmdEmbed(p paths, rest []string) {
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
}
