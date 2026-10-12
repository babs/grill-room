//go:build !windows

package daemon

// restrictToOwner is a no-op: os.CreateTemp already creates the file 0600.
func restrictToOwner(string) error { return nil }
