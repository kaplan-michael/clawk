package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/clawkwork/clawk/internal/vsockclient"
)

// VZ and QEMU use the same agent protocol. The guest resolves the login
// shell from /etc/passwd, so interactive shells also work without bash.
func agentShell(vmDir, workdir string) error {
	return runAgent(vsockclient.Config{
		SocketPath: filepath.Join(vmDir, "agent.sock"), Cwd: workdir, User: GuestUser,
	})
}

func agentExec(vmDir, user string, command ...string) error {
	if len(command) == 0 {
		return fmt.Errorf("guest exec: empty command")
	}
	return runAgent(vsockclient.Config{
		SocketPath: filepath.Join(vmDir, "agent.sock"), User: user,
		Cmd: command[0], Args: command[1:],
	})
}

func runAgent(cfg vsockclient.Config) error {
	code, err := vsockclient.Run(context.Background(), cfg)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// Hooks can run for minutes (for example, pnpm install).
func agentOutput(vmDir, user string, command ...string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("guest exec: empty command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	out, code, err := vsockclient.Output(ctx, filepath.Join(vmDir, "agent.sock"), 0, user, command[0], command[1:]...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, &ExitError{Code: code}
	}
	return out, nil
}
