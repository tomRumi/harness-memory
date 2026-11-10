package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func doctorFoldersLines(t *testing.T, repo string) string {
	t.Helper()
	var lines []string
	doctorFolders(repo, func(level, what, detail string) { lines = append(lines, level+" "+what+" "+detail) })
	return strings.Join(lines, "\n")
}

// Fourth review: doctor names every project folder that is not spelled as a project name, with the fix.
func TestDoctorNamesCapitalAndInvalidFolders(t *testing.T) {
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q", "-b", "main").Run()
	if got := doctorFoldersLines(t, repo); !strings.HasPrefix(got, "OK memory folders") {
		t.Fatalf("clean repository: %q", got)
	}
	for _, d := range []string{"wiki/MyProj", "notes/MyProj", "facts/Bad Name", "wiki/fine", "wiki/.obsidian"} {
		os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755)
	}
	os.WriteFile(filepath.Join(repo, "wiki", "Readme.md"), []byte("# R\n"), 0o644) // a file is not a project folder
	got := doctorFoldersLines(t, repo)
	for _, want := range []string{
		"WARN project folder wiki/MyProj has capital letters; project names are lower-case: git mv wiki/MyProj wiki/myproj",
		"WARN project folder notes/MyProj has capital letters; project names are lower-case: git mv notes/MyProj notes/myproj",
		"WARN project folder facts/Bad Name is not a valid project name",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "wiki/fine") || strings.Contains(got, ".obsidian") || strings.Contains(got, "Readme") || strings.Contains(got, "OK ") {
		t.Errorf("too much:\n%s", got)
	}
}

// Fourth review: git ignoring notes/, wiki/ or facts/ refuses every save, so doctor says so.
func TestDoctorWarnsWhenGitIgnoresAMemoryFolder(t *testing.T) {
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q", "-b", "main").Run()
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("notes/\n"), 0o644)
	got := doctorFoldersLines(t, repo)
	if !strings.Contains(got, "WARN ignored folder git ignores notes/; every save is refused") || strings.Contains(got, "wiki/;") || strings.Contains(got, "OK ") {
		t.Errorf("got:\n%s", got)
	}
}

// The whole command prints the lines (doctor is wired to them) and a lower-case repository stays free of WARNs.
func TestDoctorPrintsTheFolderLines(t *testing.T) {
	home := t.TempDir()
	p := Paths{Repo: filepath.Join(home, "memory"), DB: filepath.Join(home, "index.db")}
	os.MkdirAll(filepath.Join(p.Repo, "wiki", "MyProj"), 0o755)
	exec.Command("git", "-C", p.Repo, "init", "-q", "-b", "main").Run()
	t.Setenv("HMS_VECTORS", "0")
	var sb strings.Builder
	Doctor(p, &sb)
	if !strings.Contains(sb.String(), "WARN  project folder") || !strings.Contains(sb.String(), "git mv wiki/MyProj wiki/myproj") {
		t.Errorf("doctor output:\n%s", sb.String())
	}
}
