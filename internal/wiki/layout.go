package wiki

import (
	"io/fs"
	"strings"
)

// Trees are the folders of the memory repository. Nothing outside them is ever indexed or committed by the service.
var Trees = []string{"wiki", "notes", "facts"}

// TempPrefix starts the name of the temporary file of an atomic write.
const TempPrefix = ".tmp-"

// Hidden says whether a file or folder name is hidden (.obsidian, .trash, a temporary file ...).
func Hidden(name string) bool { return strings.HasPrefix(name, ".") }

// InTree says whether rel (a slash path from the repository root) lies under notes/, facts/ or wiki/.
func InTree(rel string) bool {
	for _, t := range Trees {
		if strings.HasPrefix(rel, t+"/") {
			return true
		}
	}
	return false
}

// IsMemoryPath is the one rule for the name of a memory file: a .md file under notes/, facts/ or wiki/ that is not
// hidden, in a folder that is not hidden (a temporary file is hidden).
func IsMemoryPath(rel string) bool {
	if !InTree(rel) || !strings.HasSuffix(rel, ".md") {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if Hidden(seg) {
			return false
		}
	}
	return true
}

// IsMemoryFile is IsMemoryPath for a file whose type is known: a symlink (it could point outside the repository),
// a folder or a device is not a memory file.
func IsMemoryFile(rel string, mode fs.FileMode) bool { return mode.IsRegular() && IsMemoryPath(rel) }
