package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// fileAllAccess is FILE_ALL_ACCESS, "FA" in SDDL; x/sys/windows does not export it.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1FF

// 003-AC12 on Windows, where Go file modes do not apply: every state file, room tokens and the
// fallback setup token file included, carries a protected DACL with one entry, the current user.
func TestOwnerOnlyDACL(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	want := ownerOnlySDDL(user.User.Sid.String())
	dir := t.TempDir()
	tokens := NewTokens(dir)
	if err := tokens.Set(Room{Base: "https://rooms.example", ID: "abc"}, "room-secret"); err != nil {
		t.Fatal(err)
	}
	a := &Accounts{o: AccountsOptions{Paths: Paths{Config: dir, State: dir, Cache: dir}, Keyring: &memKeyring{down: true, m: map[string]string{}}, Log: zap.NewNop()}}
	storage, err := a.storeSecret("work", setupToken)
	if err != nil || storage != storageFile {
		t.Fatalf("stored %q: %v", storage, err)
	}
	for _, p := range []string{filepath.Join(dir, "tokens.json"), a.secretFile("work")} {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 1 {
			t.Fatalf("%s: DACL %s (%v), want %s", p, sd, err, want)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // the ACE's SID starts at SidStart
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(user.User.Sid) || ace.Mask != fileAllAccess {
			t.Errorf("%s: DACL %s, want %s", p, sd, want)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Errorf("%s: DACL not protected from inheritance (%v)", p, err)
		}
	}
	// The parent folder's entries, SYSTEM and Administrators among them, did not carry over.
	if sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION); err == nil {
		t.Logf("parent folder DACL: %s", sd)
	}
	if b, err := os.ReadFile(a.secretFile("work")); err != nil || !strings.Contains(string(b), setupToken) {
		t.Fatalf("the owner cannot read its token back: %v", err)
	}
}

// 003-AC12 on Windows: a setup token goes to the Windows Credential Manager, is read back from
// it, and leaves it with its account; no file holds it.
func TestCredentialManager(t *testing.T) {
	dir := t.TempDir()
	e := &Accounts{o: AccountsOptions{Paths: Paths{Config: dir, State: dir, Cache: dir}, Keyring: osKeyring{}, Now: time.Now, Log: zap.NewNop()}}
	name := "ci-" + filepath.Base(dir)
	storage, err := e.storeSecret(name, setupToken)
	if err != nil || storage != storageKeyring {
		t.Fatalf("stored %q: %v", storage, err)
	}
	t.Cleanup(func() { _ = osKeyring{}.Delete(e.keyringService(), name) })
	acc := Account{Name: name, Kind: KindSetupToken, Storage: storage}
	tok, err := (&secretSource{a: e, acc: acc}).Get(context.Background())
	if err != nil || tok.AccessToken != setupToken {
		t.Fatalf("read back: %v", err)
	}
	if grep(t, dir, setupToken) != 0 {
		t.Fatal("a file holds the token")
	}
	e.deleteSecret(acc)
	if _, err := e.readSecret(acc); err == nil {
		t.Fatal("the token outlived its account")
	}
}
