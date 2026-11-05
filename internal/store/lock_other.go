//go:build !unix

package store

// lockFile is a no-op where there is no flock: the in-process mutex is then the only lock.
func lockFile(path string) (func(), error) { return func() {}, nil }
