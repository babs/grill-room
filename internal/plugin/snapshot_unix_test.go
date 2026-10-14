//go:build unix

package plugin

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// 009-AC9: a FIFO refuses the folder, named, and is never opened.
func TestFromDirRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a"})
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600))
	_, err := FromDir(dir, DefaultMaxBytes)
	require.ErrorContains(t, err, "pipe is not a regular file")
}
