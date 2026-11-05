package wiki

import (
	"fmt"
	"regexp"
	"strings"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Project is the one rule for a project name, used by the tools, the store, the server, curate and every -p flag:
// trimmed, lower case, and a plain folder name. A name with capitals is lower-cased (-p PROJ means proj), so no
// wiki/PROJ folder can appear. An empty name is an error here; a caller that takes "no project" as "all projects" or
// as "global" says so itself (OptionalProject, store.project).
func Project(p string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(p))
	if !slugRe.MatchString(l) {
		return "", fmt.Errorf("invalid project name %q (lower-case letters, digits, dot, dash, underscore)", l)
	}
	return l, nil
}

// OptionalProject is Project for a filter: an empty name stays empty and means every project.
func OptionalProject(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", nil
	}
	return Project(p)
}
