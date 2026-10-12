package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/trust"
)

// DevKeys are the cosign public keys the host trusts for dev builds (003-AC4): one <name>.pub
// file each in Dir, added from the page or dropped there by hand.
type DevKeys struct {
	Dir string
	Log *zap.Logger
}

var devKeyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

const devKeyExt = ".pub"

// List reads the keys, skipping and logging the unreadable ones. A nil DevKeys has none.
func (k *DevKeys) List() []trust.DevKey {
	if k == nil {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(k.Dir, "*"+devKeyExt))
	slices.Sort(files)
	var out []trust.DevKey
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), devKeyExt)
		b, err := os.ReadFile(f) //nolint:gosec // the host's own key files
		if err == nil {
			var key trust.DevKey
			if key, err = trust.ParseDevKey(name, b); err == nil {
				out = append(out, key)
				continue
			}
		}
		k.Log.Warn("dev_key_unreadable", zap.String("file", f), zap.Error(err))
	}
	return out
}

// Add trusts a PEM public key under name.
func (k *DevKeys) Add(name string, pemBytes []byte) error {
	if !devKeyName.MatchString(name) {
		return errors.New("a key name is 1 to 64 letters, digits, dots, dashes or underscores")
	}
	if _, err := trust.ParseDevKey(name, pemBytes); err != nil {
		return err
	}
	path := filepath.Join(k.Dir, name+devKeyExt)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("a key named %s exists: remove it first", name)
	}
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, pemBytes)
}

// Remove stops trusting the key name.
func (k *DevKeys) Remove(name string) error {
	if !devKeyName.MatchString(name) {
		return errors.New("no such key")
	}
	err := os.Remove(filepath.Join(k.Dir, name+devKeyExt))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no such key")
	}
	return err
}
