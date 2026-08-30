//go:build linux && amd64

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Native Linux vsock is reachable by other host users. The private Unix
// proxy alone is insufficient: all privileged guest services need this
// per-sandbox credential, retained across suspend/restore.
func qemuGuestToken(vmDir string) (string, error) {
	path := filepath.Join(vmDir, "vsock-auth")
	b, err := os.ReadFile(path)
	if err == nil {
		if decoded, e := hex.DecodeString(string(b)); e != nil || len(decoded) != 32 {
			return "", fmt.Errorf("invalid QEMU guest credential in %s", path)
		}
		return string(b), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(secret[:])
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	return token, nil
}
