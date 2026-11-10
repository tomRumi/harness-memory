package curate

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory/internal/store"
)

// synBag is a fake embedder that knows meanings: every word of a concept (car, automobile, truck ...) adds to one
// axis of the vector, and words it does not know add nothing. Texts are then close without sharing a word.
type synBag struct {
	bag
	docs atomic.Int64
}

var concepts = [][]string{
	{"vehicle", "automobile", "car", "truck", "engine"},
	{"fuel", "gasoline", "petrol"},
	{"flower", "blossom", "bloom", "tulips", "roses"},
	{"port", "daemon", "loopback"},
	{"worker", "restarts", "health"},
}

func (s *synBag) vec(text string) []float32 {
	v := make([]float32, 16)
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		for i, c := range concepts {
			for _, x := range c {
				if x == w {
					v[i]++
				}
			}
		}
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 {
		v[15], n = 1, 1
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

func (s *synBag) Docs(ctx context.Context, t []string) ([][]float32, error) {
	s.docs.Add(int64(len(t)))
	out := make([][]float32, len(t))
	for i, x := range t {
		out[i] = s.vec(x)
	}
	return out, nil
}
func (s *synBag) Query(ctx context.Context, t string) ([]float32, error) { return s.vec(t), nil }

func meaningStore(t *testing.T) (*store.Store, string, *synBag) {
	st, repo := newStore(t)
	e := &synBag{}
	st.Emb = e
	putFile(t, st, "wiki/proj/Cars.md", "# Cars\n\nThe automobile burns gasoline while the engine runs.\n")
	putFile(t, st, "wiki/proj/Garden.md", "# Garden\n\nRoses and tulips blossom in the spring beds.\n")
	putFile(t, st, "wiki/proj/Ports.md", "# Ports\n\nThe daemon listens on a loopback port.\n")
	return st, repo, e
}

func titles(rs []Related) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Title)
	}
	return out
}

func TestPendingRelatedByMeaningWithoutSharedWords(t *testing.T) {
	st, repo, _ := meaningStore(t)
	// the note and the page Cars have no word in common; the note is not indexed yet, so it is embedded
	writeNote(t, repo, proj, "n1", "My car ran out of petrol on the highway.", time.Now())
	bs, err := Pending(context.Background(), st, proj, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(bs[0].Related); len(got) != 1 || got[0] != "Cars" {
		t.Fatalf("related %v, want only Cars (not Garden or Ports)", got)
	}
}

func TestPendingRelatedRankedByVotesAndUsesStoredNoteVectors(t *testing.T) {
	st, _, e := meaningStore(t)
	ctx := context.Background()
	for _, text := range []string{"The truck needs more petrol.", "Cleaning the car every week.", "Roses bloom early this year."} {
		if _, err := st.Learn(ctx, text, proj, "learning", ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	st.WaitEmbed() // the background pass for the notes just learned must be over before the counter is reset
	e.docs.Store(0)
	bs, err := Pending(ctx, st, proj, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(bs[0].Related); fmt.Sprint(got) != "[Cars Garden]" { // two votes, then one; Ports gets none
		t.Fatalf("related %v", got)
	}
	if n := e.docs.Load(); n != 0 {
		t.Fatalf("notes with stored vectors must not be embedded again, %d texts were", n)
	}
}

func TestPendingRelatedFallsBackToKeywordsWithoutVectors(t *testing.T) {
	st, repo := newStore(t)
	putFile(t, st, "wiki/proj/Databases.md", "# Databases\n\nPostgres vacuum notes.\n")
	writeNote(t, repo, proj, "n1", "Run the postgres vacuum nightly.", time.Now())
	st.Emb = nil // no embedder: the old keyword choice
	bs, err := Pending(context.Background(), st, proj, 0)
	if err != nil || fmt.Sprint(titles(bs[0].Related)) != "[Databases]" {
		t.Fatalf("%v %+v", err, bs[0].Related)
	}
}
