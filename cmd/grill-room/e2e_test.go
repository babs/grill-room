//go:build !e2e

package main

import (
	"testing"

	"go.uber.org/zap"
)

// The release build has no way to loosen the plugin fetch guard: the e2e variable does nothing.
func TestPluginGuardIgnoresE2EVariable(t *testing.T) {
	t.Setenv("GRILL_ROOM_TEST_GIT_ALLOW_LOOPBACK", "1")
	t.Setenv("GRILL_ROOM_TEST_GIT_CA", "ca.pem")
	if c := testPluginClient(zap.NewNop()); c != nil {
		t.Fatal("the default build set a plugin client")
	}
}
