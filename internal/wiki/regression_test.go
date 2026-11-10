package wiki

import (
	"strings"
	"testing"

	"github.com/tomRumi/harness-memory/internal/md"
)

func TestValidTitleBytes(t *testing.T) {
	// 66 runes of 3 bytes: 198 bytes, accepted; 67 runes: 201 bytes, refused although far under 100 runes... of 100
	if err := ValidTitle(strings.Repeat("語", 66)); err != nil {
		t.Fatal(err)
	}
	if err := ValidTitle(strings.Repeat("語", 67)); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("201 bytes accepted: %v", err)
	}
	if err := ValidTitle(strings.Repeat("a", 100)); err != nil {
		t.Fatal(err)
	}
	if err := ValidTitle(strings.Repeat("a", 101)); err == nil {
		t.Fatal("101 characters accepted")
	}
}

func TestFoldIsNFCAndCase(t *testing.T) {
	if Fold("Cafe\u0301") != Fold("CAF\u00c9") {
		t.Fatal("NFD and NFC differ")
	}
	if Fold("Ports") == Fold("Port") {
		t.Fatal("different words fold together")
	}
}

func TestSourcesThatYAMLReadsAsMapsAreShownAsText(t *testing.T) {
	src := "---\ntitle: P\nsources:\n- 2026-10-01: abc\n- \"2026-10-02: def\"\n- 2026-10-03\n- k: v\n---\nbody\n"
	m := ParseMeta(md.Parse([]byte(src)), "P")
	if got := strings.Join(m.Sources, " | "); got != "2026-10-01: abc | 2026-10-02: def | 2026-10-03 | k: v" {
		t.Fatalf("sources %q", got)
	}
}

// Fold is what the file system of a Mac does with names: NFC and full case folding.
func TestFoldIsFullCaseFoldingOfNFC(t *testing.T) {
	for _, c := range [][2]string{
		{"Strasse", "Straße"}, {"ΟΔΟΣ", "οδος"}, {"ΟΔΟΣ", "οδοσ"}, {"file", "ﬁle"}, {"µ", "μ"}, {"ſ", "s"},
		{"Café", "Cafe\u0301"}, {"CAFÉ", "cafe\u0301"}, {"Ports", "PORTS"},
	} {
		if Fold(c[0]) != Fold(c[1]) {
			t.Errorf("Fold(%q) != Fold(%q)", c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"Cafe", "Café"}, {"Ports", "Port"}, {"a b", "ab"}} {
		if Fold(c[0]) == Fold(c[1]) {
			t.Errorf("Fold(%q) == Fold(%q)", c[0], c[1])
		}
	}
	if ValidTitle("INDEX") == nil || ValidTitle("ınDEX") == nil && Fold("ınDEX") == Fold("INDEX") {
		t.Error("Index is reserved in every folded spelling")
	}
}
