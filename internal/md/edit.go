package md

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var plainScalar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:/-]*$`)

// scalar renders a value for a frontmatter line (a []string as a flow list): plain when that is safe and unambiguous, otherwise double-quoted
// (JSON string syntax is valid YAML). A string that looks like a number, a boolean or null is quoted so it stays a string.
func scalar(v any) string {
	switch x := v.(type) {
	case string:
		if plainScalar.MatchString(x) && !regexp.MustCompile(`^(?i:true|false|null|yes|no|on|off|~|[-+]?[0-9][0-9_.,:e-]*)$`).MatchString(x) {
			return x
		}
		return fmt.Sprintf("%q", x)
	case bool:
		return fmt.Sprint(x)
	case []string: // a flow list; JSON strings are valid YAML
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.Encode(x)
		return strings.TrimSpace(b.String())
	default:
		return fmt.Sprint(x)
	}
}

// SetKeys changes or adds top-level keys in the frontmatter of src and leaves every other byte alone: comments, key
// order, quoting, line endings and the body stay as they were. A file without frontmatter gets a new block. A file
// whose frontmatter does not parse is refused (an error), never rewritten.
func SetKeys(src []byte, set map[string]any) ([]byte, error) {
	text := string(src)
	nl := "\n"
	if i := strings.Index(text, "\n"); i > 0 && text[i-1] == '\r' { // the opening delimiter line decides, not the body
		nl = "\r\n"
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := func() []string {
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			out = append(out, k+": "+scalar(set[k]))
		}
		return out
	}
	if !strings.HasPrefix(text, "---"+nl) && !strings.HasPrefix(text, "---\n") {
		return []byte("---" + nl + strings.Join(lines(), nl) + nl + "---" + nl + text), nil
	}
	if d := Parse(src); d.Error != "" {
		return nil, fmt.Errorf("frontmatter does not parse (%s): not rewritten", d.Error)
	}
	rest := text[strings.Index(text, "\n")+1:]
	headEnd := len(text) - len(rest)
	// find the closing delimiter line
	pos, closeAt := 0, -1
	for pos < len(rest) {
		end := strings.Index(rest[pos:], "\n")
		var line string
		if end < 0 {
			line, end = rest[pos:], len(rest)-pos
		} else {
			line = rest[pos : pos+end]
		}
		if strings.TrimRight(line, "\r") == "---" {
			closeAt = pos
			break
		}
		pos += end + 1
	}
	if closeAt < 0 {
		return nil, fmt.Errorf("frontmatter is not closed: not rewritten")
	}
	block := rest[:closeAt]
	bl := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	if block == "" {
		bl = nil
	}
	done := map[string]bool{}
	drop := map[int]bool{} // continuation lines of a value that was replaced
	for i := 0; i < len(bl); i++ {
		l := bl[i]
		for _, k := range keys {
			if done[k] || !(strings.HasPrefix(l, k+":") || strings.HasPrefix(l, `"`+k+`":`) || strings.HasPrefix(l, "'"+k+"':")) {
				continue
			}
			cr := ""
			if strings.HasSuffix(l, "\r") {
				cr = "\r"
			}
			bl[i] = k + ": " + scalar(set[k]) + cr // a quoted key becomes a plain one: the same key, once
			done[k] = true
			// a block or folded value goes on over indented lines; they belong to the old value. When the key has no
			// value on its own line it is a block list, whose "- " lines may also start at column 0.
			blockList := emptyValue(l)
			for j := i + 1; j < len(bl); j++ {
				c := strings.TrimRight(bl[j], "\r")
				if strings.HasPrefix(c, " ") || strings.HasPrefix(c, "\t") || blockList && (c == "-" || strings.HasPrefix(c, "- ")) {
					drop[j] = true
					continue
				}
				break
			}
		}
	}
	if len(drop) > 0 {
		kept := bl[:0:0]
		for i, l := range bl {
			if !drop[i] {
				kept = append(kept, l)
			}
		}
		bl = kept
	}
	for _, k := range keys {
		if !done[k] {
			add := k + ": " + scalar(set[k])
			if nl == "\r\n" {
				add += "\r"
			}
			bl = append(bl, add)
		}
	}
	var b strings.Builder
	b.WriteString(text[:headEnd])
	for _, l := range bl {
		b.WriteString(l + "\n")
	}
	b.WriteString(rest[closeAt:])
	return []byte(b.String()), nil
}

// emptyValue reports whether the key line l has no value after the colon (nothing, or only a comment).
func emptyValue(l string) bool {
	_, rest, _ := strings.Cut(l, ":")
	rest = strings.TrimSpace(rest)
	return rest == "" || strings.HasPrefix(rest, "#")
}
