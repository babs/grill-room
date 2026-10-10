//go:build !darwin

package creds

// platformKeychain is nil: only macOS has a Keychain, elsewhere Claude Code uses the file.
var platformKeychain KeychainFunc
