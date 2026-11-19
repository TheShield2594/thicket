//go:build unix

package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// runGroup runs argv in its own process group and, when it exits or ctx
// ends, kills the whole group, so nothing it started keeps running.
func runGroup(ctx context.Context, argv []string, dir string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = append(os.Environ(), "TERM=dumb", "PAGER=cat", "GIT_PAGER=cat", "DEBIAN_FRONTEND=noninteractive")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Background children can hold stdout open; don't wait on them.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // leftovers
	if ctx.Err() != nil {
		return -1, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return cmd.ProcessState.ExitCode(), nil
	}
	return 0, err
}
