// Package install holds the install behaviour: `hms init` (create the memory repository and index, check the
// embedder, offer to pull the model) and `hms doctor` (the same checks, read-only). It never installs a program.
package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

// Paths are the three locations of a memory.
type Paths struct{ Home, Repo, DB string }

// VectorsEnabled is true unless HMS_VECTORS=0.
func VectorsEnabled() bool { return os.Getenv("HMS_VECTORS") != "0" }

// InitOptions controls Init.
type InitOptions struct {
	Yes         bool      // answer yes to the download question
	Interactive bool      // standard input is a terminal, so the question can be asked
	In          io.Reader // where the answer is read from
	Out         io.Writer
	Check       time.Duration // limit for each embedder call; 0 means 30 s
}

// NoEmbedderLine is the one line printed when no embedder answers.
func NoEmbedderLine(cfg embed.Config) string {
	if cfg.API == "openai" {
		return "vectors: unavailable: no embedding answer from HMS_EMBED_URL (check the URL, HMS_EMBED_MODEL and HMS_EMBED_KEY); keyword match only until it works"
	}
	return "vectors: unavailable: no embedder found; install Ollama (https://ollama.com) and run: ollama pull " + cfg.Model +
		", or set HMS_EMBED_URL for another server. Keyword match only until then"
}

// Init creates the memory repository (with a local git repository), the index and the settings, then checks the
// embedder. Running it again changes nothing. It returns an error only when the repository or index cannot be made;
// an embedder that is missing or unwilling is reported and is not an error.
func Init(p Paths, o InitOptions) (err error) {
	commitErr, err := prepare(p, o.Out)
	if err != nil {
		return err
	}
	defer func() { // a failed first commit is reported after the rest of init ran; the files are saved
		if err == nil {
			err = commitErr
		}
	}()
	cfg := embed.Default()
	if !VectorsEnabled() {
		fmt.Fprintln(o.Out, "vectors: off (HMS_VECTORS=0)")
		return nil
	}
	lim := o.Check
	if lim == 0 {
		lim = 30 * time.Second
	}
	c := embed.New(cfg)
	st := check(c, lim)
	switch st.State {
	case embed.Ready:
		fmt.Fprintf(o.Out, "vectors: on (%s)\n", cfg.Model)
	case embed.Unreachable:
		fmt.Fprintln(o.Out, NoEmbedderLine(cfg))
	case embed.ModelMissing:
		pullOffer(c, cfg, o, lim)
	}
	return nil
}

func check(c *embed.Client, lim time.Duration) embed.Status {
	ctx, cancel := context.WithTimeout(context.Background(), lim)
	defer cancel()
	return c.Check(ctx)
}

func pullOffer(c *embed.Client, cfg embed.Config, o InitOptions, lim time.Duration) {
	cmd := "ollama pull " + cfg.Model
	fmt.Fprintf(o.Out, "the model %s is not on the server at %s (%s)\n", cfg.Model, cfg.URL, embed.ModelSize(cfg.Model))
	yes := o.Yes
	switch {
	case yes:
	case !o.Interactive:
		fmt.Fprintf(o.Out, "not downloading without a terminal or --yes; to get it run: %s\n", cmd)
		fmt.Fprintln(o.Out, "vectors: unavailable: model missing; keyword match only until then")
		return
	default:
		fmt.Fprint(o.Out, "Download it now? [y/N] ")
		line, _ := bufio.NewReader(o.In).ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		yes = a == "y" || a == "yes"
	}
	if !yes {
		fmt.Fprintf(o.Out, "not downloaded; to get it later run: %s\n", cmd)
		fmt.Fprintln(o.Out, "vectors: unavailable: model missing; keyword match only until then")
		return
	}
	fmt.Fprintf(o.Out, "downloading %s through the server (this can take a while)...\n", cfg.Model)
	if err := c.Pull(context.Background()); err != nil {
		fmt.Fprintf(o.Out, "download failed: %s\nrun it yourself: %s\nvectors: unavailable: model missing; keyword match only until then\n", firstLine(err), cmd)
		return
	}
	if st := check(c, lim); st.State == embed.Ready {
		fmt.Fprintf(o.Out, "downloaded. vectors: on (%s)\n", cfg.Model)
	} else {
		fmt.Fprintf(o.Out, "the download finished but the model does not answer yet; vectors: unavailable (keyword match only)\n")
	}
}

func firstLine(err error) string { return strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0]) }

// prepare makes the directories, the .gitignore, the git repository and the index; each only if missing.
func prepare(p Paths, out io.Writer) (commitErr, err error) {
	for _, d := range []string{"wiki", "notes", "facts"} {
		if err := os.MkdirAll(filepath.Join(p.Repo, d), 0o755); err != nil {
			return nil, err
		}
	}
	gi := filepath.Join(p.Repo, ".gitignore")
	madeIgnore := false
	if _, err := os.Stat(gi); os.IsNotExist(err) {
		if err := os.WriteFile(gi, []byte("*.db\n*.db-shm\n*.db-wal\n"), 0o644); err != nil {
			return nil, err
		}
		madeIgnore = true
	}
	if _, err := os.Stat(filepath.Join(p.Repo, ".git")); os.IsNotExist(err) {
		if _, err := exec.LookPath("git"); err != nil {
			fmt.Fprintln(out, "warning: the git program is not installed; notes will be saved as files without commits")
		} else if o, err := store.Git(p.Repo, "init", "-q", "-b", "main"); err != nil {
			fmt.Fprintf(out, "warning: git init failed (%v): %s\n", err, o)
		} else if madeIgnore {
			// the initial commit holds the .gitignore, so `git status` is clean after init. When it fails nothing stays
			// staged: a staged .gitignore would make the committer refuse every later memory commit.
			o, err := store.Git(p.Repo, "add", "--", ".gitignore")
			if err == nil {
				o, err = store.Git(p.Repo, "-c", "user.name=hms", "-c", "user.email=hms@localhost", "commit", "-q", "-m", "hms init", "--", ".gitignore")
			}
			if err != nil {
				store.Git(p.Repo, "reset", "-q", "--", ".gitignore")
				commitErr = fmt.Errorf("committed: NO (the first commit failed: %v: %s); the files are saved, run hms doctor", err, firstLine(errors.New(o)))
			}
		}
	}
	d, err := index.Open(p.DB)
	if err != nil {
		return nil, fmt.Errorf("open index %s: %w", p.DB, err)
	}
	d.Close()
	fmt.Fprintf(out, "memory repository: %s\nindex:             %s\n", p.Repo, p.DB)
	return commitErr, nil
}
