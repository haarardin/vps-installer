//go:build integration && linux

package integration

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/haarardin/vps-installer/internal/app"
	"github.com/haarardin/vps-installer/internal/compose"
	"github.com/haarardin/vps-installer/internal/spec"
	"github.com/haarardin/vps-installer/internal/state"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}
func TestRealLifecycle(t *testing.T) {
	if os.Getenv("ENVCTL_INTEGRATION") != "1" {
		t.Skip("set ENVCTL_INTEGRATION=1 on an isolated Docker test host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	runner := compose.Process{}
	rt := compose.Runtime{Runner: runner, Context: "default"}
	a := app.App{Store: state.Store{Root: t.TempDir()}, Runtime: rt, WaitSeconds: 120}
	s := spec.Spec{APIVersion: spec.APIVersion, Name: "integration", Services: []spec.Service{{Name: "db", Template: "postgres", Version: "17", Persistent: true}, {Name: "cache", Template: "redis", Version: "7", HostPort: freePort(t)}}}
	p, err := a.Plan(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	project := app.Project(p.EnvironmentID)
	dir, _ := a.Store.Dir(s.Name)
	manifest := filepath.Join(dir, "compose.json")
	// Cleanup is explicitly limited to this random test project, including its test-only volume.
	t.Cleanup(func() {
		cleanup, c := context.WithTimeout(context.Background(), time.Minute)
		defer c()
		if err := rt.Down(cleanup, project, manifest); err != nil {
			t.Log("cleanup:", err)
		}
		if _, err := runner.Run(cleanup, "--context", "default", "volume", "rm", project+"_db-data"); err != nil {
			t.Log("test volume cleanup:", err)
		}
	})
	if err = a.Apply(ctx, a.PlanPath(p)); err != nil {
		t.Fatal(err)
	}
	sql := func(q string) string {
		t.Helper()
		b, err := runner.Run(ctx, "--context", "default", "compose", "--project-name", project, "--file", manifest, "exec", "-T", "db", "psql", "-U", "app", "-d", "app", "-At", "-v", "ON_ERROR_STOP=1", "-c", q)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	sql("CREATE TABLE marker (value text); INSERT INTO marker VALUES ('survived');")
	password, err := state.ReadBytes(filepath.Join(dir, "secrets", "cache.password"), 128)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.Services[1].HostPort), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(connection)
	for _, cmd := range [][]string{{"AUTH", string(password)}, {"SET", "marker", "works"}, {"GET", "marker"}} {
		if _, err = fmt.Fprintf(connection, "*%d\r\n", len(cmd)); err != nil {
			t.Fatal(err)
		}
		for _, arg := range cmd {
			if _, err = fmt.Fprintf(connection, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
				t.Fatal(err)
			}
		}
		line, e := reader.ReadString('\n')
		if e != nil {
			t.Fatal(e)
		}
		if strings.HasPrefix(line, "-") {
			t.Fatal("Redis command failed")
		}
		if cmd[0] == "GET" {
			value, e := reader.ReadString('\n')
			if e != nil || value != "works\r\n" {
				t.Fatal("Redis value mismatch", e)
			}
		}
	}
	before, err := a.Status(ctx, s.Name)
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.Plan(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Apply(ctx, a.PlanPath(again)); err != nil {
		t.Fatal(err)
	}
	after, err := a.Status(ctx, s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Hash(before.Resources.Stable()) != spec.Hash(after.Resources.Stable()) {
		t.Fatal("unchanged apply replaced resources")
	}
	down, err := a.PlanDown(ctx, s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Apply(ctx, a.PlanPath(down)); err != nil {
		t.Fatal(err)
	}
	up, err := a.Plan(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Apply(ctx, a.PlanPath(up)); err != nil {
		t.Fatal(err)
	}
	if got := sql("SELECT value FROM marker"); got != "survived" {
		t.Fatal("persistent data lost")
	}
	newPassword, err := state.ReadBytes(filepath.Join(dir, "secrets", "cache.password"), 128)
	if err != nil || string(newPassword) != string(password) {
		t.Fatal("credentials changed")
	}
}
