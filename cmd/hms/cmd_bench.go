package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
)

// cmdBench times recall over the questions of the given files.
func cmdBench(p paths, rest []string) {
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
	benchReport(d, m, queries, warm, cold, loaded)
}

func benchReport(d *index.DB, m *index.Matrix, queries []string, warm, cold []time.Duration, loaded time.Duration) {
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
