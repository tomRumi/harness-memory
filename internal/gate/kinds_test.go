package gate

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Cases written after the approver's second review, one per kind of value it found missing. They are the builder's
// own wording; the approver writes its own cases at each review.
func TestKindsOfChangeAreRefused(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{"environment variable name", "Export GOFLAGS before the build so the vendor directory is used by every step.", "Export GOPROXY before the build so the vendor directory is used by every step."},
		{"SQL string in single quotes", "Select the rows where the status column equals 'draft' and nothing else at all.", "Select the rows where the status column equals 'drafts' and nothing else at all."},
		{"unit changes from GHz to MHz", "The board runs the main core at 4.5 GHz when the fan profile is set to full.", "The board runs the main core at 4.5 MHz when the fan profile is set to full."},
		{"percentile p95 to p99", "The alert fires when the p95 latency of the recall call stays above the limit.", "The alert fires when the p99 latency of the recall call stays above the limit."},
		{"negative number loses its sign", "Set the offset to -1 so the first row is skipped by the reader every time.", "Set the offset to 1 so the first row is skipped by the reader every time."},
		{"IPv6 group changes", "The listener binds to 2001:db8::8a2e and refuses every other address at start.", "The listener binds to 2001:db8::8a2f and refuses every other address at start."},
		{"Windows drive letter", "The installer writes its cache to D:\\Cache and never touches the system drive.", "The installer writes its cache to E:\\Cache and never touches the system drive."},
		{"mail address", "Send the weekly report to owner@example.test and to nobody else on the list.", "Send the weekly report to admin@example.test and to nobody else on the list."},
		{"model tag", "Pull gemma4:12b for the local test and keep the larger one off this machine.", "Pull gemma4:27b for the local test and keep the larger one off this machine."},
		{"command flag", "Push with --force-with-lease only, never with the plain force option on a shared branch.", "Push with --force only, never with the plain force option on a shared branch."},
		{"boolean value", "Call the loader with explicit_only=True so that the defaults are never merged in.", "Call the loader with explicit_only=False so that the defaults are never merged in."},
		{"path extension after a date", "The log is written to /var/log/hms/2026-11-05.json and rotated once every day.", "The log is written to /var/log/hms/2026-11-05.yaml and rotated once every day."},
		{"time of day", "The backup starts at 09:20 and has to be finished before the shop opens again.", "The backup starts at 09:21 and has to be finished before the shop opens again."},
		{"short quoted string", "The flag takes the exact word \"on\" and anything else is treated as switched off.", "The flag takes the exact word \"no\" and anything else is treated as switched off."},
		{"relative path loses its parent dots", "Load the settings from ../config/app.yaml before anything else starts to run.", "Load the settings from ./config/app.yaml before anything else starts to run."},
		{"number at the start of a wrapped line", "The shop is open every day, so the batch runs\n7. times a week without a break at the weekend.", "The shop is open every day, so the batch runs\n9. times a week without a break at the weekend."},
		{"curly quotes then changed", "The robots value is \u201cnoindex\u201d on staging pages and \u201cindex\u201d on live pages.", "The robots value is \u201cnoindex, nofollow\u201d on staging pages and \u201cindex\u201d on live pages."},
	}
	for _, c := range cases {
		if ok, _ := CheckReplace(c.old, c.new); ok {
			t.Errorf("replace accepted: %s", c.name)
		}
		n := []Note{{"aaaaaaaa-0000-0000-0000-000000000000", c.old}}
		if ok, _ := CheckAppend(n, "- "+c.new+" [[aaaaaaaa]]"); ok {
			t.Errorf("append accepted: %s", c.name)
		}
	}
}

func TestHarmlessRewritesAreAccepted(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{"straight quotes become curly", "The value is \"noindex\" on staging pages and nothing else is allowed there.", "The value is \u201cnoindex\u201d on staging pages and nothing else is allowed there."},
		{"numbered list renumbered and wrapped", "1. Stop the daemon\n2. Copy the file to /srv/app/config.yaml\n3. Start the daemon", "1. Stop the daemon\n2. Copy the file to\n   /srv/app/config.yaml\n3. Start the daemon"},
		{"bullets become numbered", "- Stop the daemon first\n- Copy the file to /srv/app/config.yaml\n- Start the daemon again", "1. Stop the daemon first\n2. Copy the file to /srv/app/config.yaml\n3. Start the daemon again"},
	}
	for _, c := range cases {
		if ok, p := CheckReplace(c.old, c.new); !ok {
			t.Errorf("replace refused: %s: %v", c.name, p)
		}
	}
}

func TestCitationInASubBulletKeepsTheValues(t *testing.T) {
	note := Note{"bbbbbbbb-0000-0000-0000-000000000000", "The nightly job copies the dump to /srv/backup/hms and keeps 7 copies in total."}
	section := "- The nightly job copies the dump to /srv/backup/hms and keeps 7 copies in total.\n  - source [[bbbbbbbb]]\n"
	if ok, p := CheckAppend([]Note{note}, section); !ok {
		t.Fatalf("a citation on an indented sub-bullet must count for its item: %v", p)
	}
}

func TestAValueOnlyInACodeFenceDoesNotStandForProse(t *testing.T) {
	old := "The retry limit is 3 attempts per run, set in the job file."
	nw := "The retry limit is 5 attempts per run, set in the job file.\n\n```\nretries: 3\n```\n"
	if ok, _ := CheckReplace(old, nw); ok {
		t.Fatal("the old value inside a code fence must not hide a changed value in the prose")
	}
}

func TestLongNotesAreCheckedInReasonableTime(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "Line %d says that `value_%d` is 10.0.%d.%d on port %d and /srv/dir%d/file.json holds it.\n", i, i, i%200, i%250, 8000+i%900, i)
	}
	big := b.String() // about 300 KB
	start := time.Now()
	CheckReplace(big, strings.ReplaceAll(big, "`", ""))
	CheckAppend([]Note{{"cccccccc-0000-0000-0000-000000000000", big}}, "- unrelated text [[cccccccc]]")
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("300 KB took %s (about 1 s without -race; 12 s with it)", d)
	}
}

func TestASubBulletWithItsOwnTextDoesNotCoverItsParent(t *testing.T) {
	a := Note{"aaaaaaaa-0000-0000-0000-000000000000", "The daemon listens on port 8765 and nowhere else on this machine at all."}
	b := Note{"bbbbbbbb-0000-0000-0000-000000000000", "The old daemon listened on port 8765 until the move was done last month."}
	section := "- The daemon listens on port 8766 and nowhere else on this machine at all. [[aaaaaaaa]]\n  - The old daemon listened on port 8765 until the move was done last month. [[bbbbbbbb]]\n"
	if ok, _ := CheckAppend([]Note{a, b}, section); ok {
		t.Fatal("note A's changed port was hidden by the port in note B's sub-bullet")
	}
}

func TestListNumbersAreOnlyListNumbers(t *testing.T) {
	if got := stripListMarks("1. a\n2. b\n   wrapped\n3. c"); got != "a\nb\n   wrapped\nc" {
		t.Fatalf("%q", got)
	}
	if got := stripListMarks("is\n7. days\n2026. november"); !strings.Contains(got, "7. days") || !strings.Contains(got, "2026. november") {
		t.Fatalf("%q", got)
	}
}
