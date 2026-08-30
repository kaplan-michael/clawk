package agentembed

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInitAccounts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("guest init tests run natively on Linux")
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), InitMainGo)
	mustWrite(t, filepath.Join(dir, "go.mod"), InitGoMod)
	tests, err := os.ReadFile("init_accounts_test.go.in")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "accounts_test.go"), tests)
	if out, err := runIn(t, dir, "go", "mod", "tidy"); err != nil {
		t.Fatalf("tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "-count=1", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guest identity tests: %v\n%s", err, out)
	}
}
