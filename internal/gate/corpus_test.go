package gate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
)

// rewrap joins the lines of plain paragraphs (outside code fences) into one line and leaves everything else alone.
func rewrap(t string) string {
	var out []string
	inFence := false
	plainLine := func(l string) bool {
		return strings.TrimSpace(l) != "" && !strings.HasPrefix(strings.TrimSpace(l), "```") && !strings.ContainsAny(l[:1], "#-*+|>0123456789 \t")
	}
	for _, l := range strings.Split(t, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
			out = append(out, l)
			continue
		}
		if !inFence && plainLine(l) && len(out) > 0 && plainLine(out[len(out)-1]) {
			out[len(out)-1] += " " + l
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func TestKindsOfValueSurviveIdentityAndRewrap(t *testing.T) {
	text := "# Notes\n\nA run takes ≈40 s and the long one ~1.5 min. The 402-line prompt lives in `~/.hermes/logs/update_receipts/latest.json`.\nRun `git pull && ./relink.sh` from /srv/app, expires 2026-12-01 at 10.0.0.1:8765, commit 3fe598d.\nSet \"noindex\" on staging. See https://example.org/a/b?c=1 and `core.py:345–366`.\n"
	if ok, p := CheckReplace(text, text); !ok {
		t.Fatalf("identity rejected: %v", p)
	}
	if ok, p := CheckReplace(text, rewrap(text)); !ok {
		t.Fatalf("rewrap rejected: %v", p)
	}
}

// With HMS_GATE_CORPUS=<directory of markdown files> this checks every file against itself and against a rewrapped
// copy: a faithful rewrite must never be refused. It is skipped when the variable is not set.
func TestCorpusIdentityAndRewrap(t *testing.T) {
	dir := os.Getenv("HMS_GATE_CORPUS")
	if dir == "" {
		t.Skip("HMS_GATE_CORPUS not set")
	}
	files, bad, changed := 0, 0, 0
	var slowest time.Duration
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, _ := os.ReadFile(p)
		txt := string(b)
		re := rewrap(txt)
		if re != txt {
			changed++
		}
		files++
		start := time.Now()
		for _, cand := range []string{txt, re} {
			if ok, probs := CheckReplace(txt, cand); !ok {
				bad++
				if bad <= 5 {
					t.Errorf("%s refused: %.200s", filepath.Base(p), strings.Join(probs, " | "))
				}
			}
		}
		if d := time.Since(start); d > slowest {
			slowest = d
		}
		return nil
	})
	t.Logf("files %d, rewrapped copies differing %d, refused %d (of %d checks), slowest file %s", files, changed, bad, 2*files, slowest)
}

// mutate changes the last letter or digit of v to a different one.
func mutate(v string) string {
	r := []rune(v)
	for i := len(r) - 1; i >= 0; i-- {
		switch {
		case r[i] >= '0' && r[i] <= '8':
			r[i]++
			return string(r)
		case r[i] == '9':
			r[i] = '0'
			return string(r)
		case r[i] >= 'a' && r[i] < 'z':
			r[i]++
			return string(r)
		case r[i] == 'z':
			r[i] = 'a'
			return string(r)
		case r[i] >= 'A' && r[i] < 'Z':
			r[i]++
			return string(r)
		}
	}
	return v + "x"
}

// With HMS_GATE_CORPUS set, every hard value of every file is changed by one character, in every place where it
// occurs, and the gate must refuse the result. A value that is also a part of a longer value that stays (for example
// "80" inside "8080") is not counted: changing it is not a change of that value alone.
func TestCorpusSingleCharacterChangesAreRefused(t *testing.T) {
	dir := os.Getenv("HMS_GATE_CORPUS")
	if dir == "" {
		t.Skip("HMS_GATE_CORPUS not set")
	}
	tried, missed := 0, 0
	byKind := map[string][2]int{}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, _ := os.ReadFile(p)
		txt := string(b)
		vals := HardValues(txt)
		step := 1
		if len(vals) > 40 {
			step = len(vals) / 40 // at most about 40 values per file, spread over the file
		}
		for i := 0; i < len(vals); i += step {
			v := vals[i]
			if !strings.Contains(txt, v) {
				continue // a value that was normalised away from the text (quotes, trimmed punctuation)
			}
			mut := strings.ReplaceAll(txt, v, mutate(v))
			if mut == txt {
				continue
			}
			// the changed text must no longer hold the value as a whole value; otherwise the change is not a loss
			if newHolder(mut).set[v] || strings.Contains(flat(mut), v) {
				continue // the value is still there, for example wrapped over a line break somewhere else
			}
			tried++
			kind := "other"
			switch {
			case strings.Contains(v, "/"):
				kind = "path"
			case strings.Trim(v, "0123456789.,") == "":
				kind = "number"
			case strings.ContainsAny(v, " "):
				kind = "phrase"
			}
			k := byKind[kind]
			k[0]++
			if ok, _ := CheckReplace(txt, mut); ok {
				missed++
				k[1]++
				if missed <= 8 {
					t.Errorf("%s: %q -> %q accepted", filepath.Base(p), v, mutate(v))
				}
			}
			byKind[kind] = k
		}
		return nil
	})
	t.Logf("single-character changes tried %d, accepted by the gate %d; by kind (tried, accepted): %v", tried, missed, byKind)
}

var listNumberRe = regexp.MustCompile(`^\(?\d+[.)]$`)

// With HMS_GATE_CORPUS set, this changes whole white-space tokens chosen WITHOUT using the extractor: every token
// that holds a digit, @, backslash, colon, underscore, a leading dash, or five or more capitals. It finds kinds of
// value the extractor does not know. A token that is only a list number is skipped (renumbering is layout).
func TestCorpusTokenChangesAreRefused(t *testing.T) {
	dir := os.Getenv("HMS_GATE_CORPUS")
	if dir == "" {
		t.Skip("HMS_GATE_CORPUS not set")
	}
	tried, missed := 0, 0
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, _ := os.ReadFile(p)
		txt := string(b)
		prose, _ := splitFences(txt)
		seen := map[string]bool{}
		var toks []string
		for _, f := range strings.Fields(prose) {
			c := strings.Trim(f, "()[]{}<>\"'`.,;!?*|:")
			if c == "" || seen[c] || listNumberRe.MatchString(f) || !strings.ContainsFunc(c, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
				continue
			}
			if strings.ContainsAny(c, "0123456789@\\_") || strings.Contains(c, ":") || (c[0] == '-' && len(c) > 1) || (len(c) >= 5 && strings.Trim(c, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "") {
				seen[c] = true
				toks = append(toks, c)
			}
		}
		step := 1
		if len(toks) > 40 {
			step = len(toks) / 40
		}
		for i := 0; i < len(toks); i += step {
			v := toks[i]
			mut := strings.ReplaceAll(txt, v, mutate(v))
			if mut == txt || strings.Contains(flat(mut), v) {
				continue // nothing changed, or the token still occurs somewhere (inside a longer token or wrapped)
			}
			tried++
			if ok, _ := CheckReplace(txt, mut); ok {
				missed++
				t.Logf("accepted, to be read by a person: %s: token %q -> %q", filepath.Base(p), v, mutate(v))
			}
		}
		return nil
	})
	t.Logf("token changes tried %d, accepted by the gate %d", tried, missed)
	// A change that leaves the same value elsewhere in the file is not a loss (an equivalent change), so a few
	// acceptances are expected; more than one in a thousand means a kind of value is not recognised.
	if missed*1000 > tried {
		t.Fatalf("%d of %d token changes accepted", missed, tried)
	}
}
