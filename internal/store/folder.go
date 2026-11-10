package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tomRumi/harness-memory/internal/wiki"
)

// OddFolders lists the entries of tree (wiki, notes or facts) that are not spelled as a canonical project name: names
// with capitals or invalid characters. Hidden entries and plain files are skipped, as everywhere else.
func OddFolders(repo, tree string) []string {
	es, _ := os.ReadDir(filepath.Join(repo, tree))
	var odd []string
	for _, e := range es {
		if n := e.Name(); e.IsDir() && !strings.HasPrefix(n, ".") {
			if c, err := wiki.Project(n); err != nil || c != n {
				odd = append(odd, n)
			}
		}
	}
	return odd
}

// checkFolder refuses when tree/proj exists on disk under a name that matches proj case-insensitively but is spelled
// differently (on a case-insensitive file system that is the same folder, on a case-sensitive one a second folder).
// The parent directory is listed, so the names are compared byte for byte, whatever the file system does.
func checkFolder(repo, tree, proj string) error {
	es, _ := os.ReadDir(filepath.Join(repo, tree))
	for _, e := range es {
		if n := e.Name(); n != proj && strings.EqualFold(n, proj) {
			return fmt.Errorf("the folder %s/%s has capital letters; project names are lower-case. Rename it to %s/%s (git mv) and run hms rescan", tree, n, tree, proj)
		}
	}
	return nil
}

// guard runs checkFolder for the project folder that rel (notes/<p>/..., facts/<p>/..., wiki/<p>/...) lies in, once
// per operation. A path outside the three trees, or without a project folder, passes.
func (tx *Tx) guard(rel string) error {
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) < 3 || !wiki.InTree(rel) {
		return nil
	}
	key := parts[0] + "/" + strings.ToLower(parts[1])
	if tx.checked[key] {
		return nil
	}
	if err := checkFolder(tx.s.Repo, parts[0], strings.ToLower(parts[1])); err != nil {
		return err
	}
	tx.checked[key] = true
	return nil
}
