//go:build linux

package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type Runner interface {
	Run(context.Context, ...string) ([]byte, error)
}
type Process struct{ Binary string }
type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remain := b.limit - b.Len()
	if n > remain {
		b.overflow = true
		p = p[:remain]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func cleanEnv() []string {
	var env []string
	for _, s := range os.Environ() {
		key := strings.SplitN(s, "=", 2)[0]
		if strings.HasPrefix(key, "COMPOSE_") || strings.HasPrefix(key, "DOCKER_") {
			continue
		}
		env = append(env, s)
	}
	// Authentication config can be explicitly selected; it must not change the target.
	if v := os.Getenv("DOCKER_CONFIG"); v != "" {
		env = append(env, "DOCKER_CONFIG="+v)
	}
	return append(env, "COMPOSE_ANSI=never", "COMPOSE_MENU=false")
}
func (p Process) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := p.Binary
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = cleanEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	stdout, stderr := &limitedBuffer{limit: 4 << 20}, &limitedBuffer{limit: 16 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("Docker command failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.overflow {
		return nil, errors.New("Docker output exceeded 4 MiB")
	}
	return stdout.Bytes(), nil
}
