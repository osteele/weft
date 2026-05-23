package r2upload

import (
	"context"
	"io"
	"os/exec"
	"syscall"
)

// ExecRunner runs rclone via os/exec. This is the production [Runner].
type ExecRunner struct {
	// Binary overrides the rclone binary path. Empty string → "rclone".
	Binary string
}

// Start implements [Runner].
func (r ExecRunner) Start(ctx context.Context, args []string) (Process, error) {
	binary := r.Binary
	if binary == "" {
		binary = "rclone"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, stderr: stderr}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stderr io.ReadCloser
}

func (p *execProcess) Stderr() io.Reader { return p.stderr }

func (p *execProcess) Wait() error {
	err := p.cmd.Wait()
	// Closing stderr is harmless if the process already closed it.
	_ = p.stderr.Close()
	return err
}

func (p *execProcess) Kill() {
	if p.cmd.Process == nil {
		return
	}
	// Kill the whole process group so any rclone-spawned helpers are torn
	// down too.
	pgid, err := syscall.Getpgid(p.cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = p.cmd.Process.Kill()
}
