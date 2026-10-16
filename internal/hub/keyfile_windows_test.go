package hub_test

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/service"
)

// On Windows the key file of hub keygen is closed to everyone but
// Administrators and SYSTEM from its creation, and the hub refuses one
// that is not (here: one that inherits its folder's access).
func TestSigningKeyFileACL(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("reading a key closed to Administrators and SYSTEM needs an elevated prompt")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "hub-signing.key")
	if _, err := hub.WriteSigningKey(key); err != nil {
		t.Fatal(err)
	}
	protected, null, aces, err := service.ReadACL(key)
	if err != nil {
		t.Fatal(err)
	}
	if msg := service.ACLProblem(protected, null, aces); msg != "" {
		t.Fatalf("the new key file: %s", msg)
	}
	cfg := &hub.Settings{StateDir: filepath.Join(dir, "state"), SigningKeyFile: key}
	s, err := hub.Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	sd, err := windows.GetNamedSecurityInfo(key, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(key, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Open(cfg, nil); err == nil || !strings.Contains(err.Error(), "icacls") {
		t.Fatalf("a key file that inherits its folder's access: %v", err)
	}
}
