package compose

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/haarardin/vps-installer/internal/spec"
)

func fixture() spec.Spec {
	return spec.Spec{APIVersion: spec.APIVersion, Name: "dev", Services: []spec.Service{{Name: "db", Template: "postgres", Version: "17", Persistent: true, HostPort: 5432}, {Name: "cache", Template: "redis", Version: "7"}}}
}
func TestRender(t *testing.T) {
	s := fixture()
	b, err := Render(s, "abc", "/private/dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	db := f.Services["db"]
	if !reflect.DeepEqual(db.Ports, []string{"127.0.0.1:5432:5432"}) {
		t.Fatal(db.Ports)
	}
	if db.Environment["POSTGRES_PASSWORD_FILE"] != "/run/secrets/db-password" {
		t.Fatal(db.Environment)
	}
	if len(db.Volumes) != 1 || len(f.Services["cache"].Tmpfs) != 1 || len(f.Services["cache"].Ports) != 0 {
		t.Fatal("storage/exposure mismatch")
	}
	if f.Networks["default"].Labels[OwnerLabel] != "abc" || f.Volumes["db-data"].Labels[OwnerLabel] != "abc" {
		t.Fatal("ownership labels missing")
	}
	if strings.Contains(string(b), "privileged") || strings.Contains(string(b), "POSTGRES_PASSWORD\"") {
		t.Fatal("unsafe output")
	}
	if _, err = Render(s, "abc", "/private/dev", map[string]string{"db": "postgres:latest"}); err == nil {
		t.Fatal("mutable image accepted")
	}
	images := map[string]string{"db": "docker.io/library/postgres@sha256:" + strings.Repeat("a", 64), "cache": "docker.io/library/redis@sha256:" + strings.Repeat("b", 64)}
	if _, err = Render(s, "abc", "/private/dev", images); err != nil {
		t.Fatal(err)
	}
	s.Services[1].Persistent = true
	b, err = Render(s, "abc", "/private/dev", nil)
	if err != nil || !strings.Contains(string(b), "cache-data") {
		t.Fatal(string(b), err)
	}
}

type fakeRunner struct {
	calls [][]string
	run   func([]string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, a ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), a...))
	if f.run != nil {
		return f.run(a)
	}
	return nil, nil
}
func TestCommands(t *testing.T) {
	f := &fakeRunner{}
	r := Runtime{f, "default"}
	ctx := context.Background()
	if err := r.Up(ctx, "project", "/tmp/compose.json", 45); err != nil {
		t.Fatal(err)
	}
	expected := []string{"--context", "default", "compose", "--env-file", "/dev/null", "--project-name", "project", "--file", "/tmp/compose.json", "up", "--wait", "--wait-timeout", "45", "--pull", "missing"}
	if !reflect.DeepEqual(f.calls[0], expected) {
		t.Fatal(f.calls[0])
	}
	if err := r.Down(ctx, "project", "/tmp/compose.json"); err != nil {
		t.Fatal(err)
	}
	for _, arg := range f.calls[1] {
		if arg == "--volumes" || arg == "-v" || arg == "--remove-orphans" {
			t.Fatal("destructive flag")
		}
	}
}
func TestResolve(t *testing.T) {
	f := &fakeRunner{run: func([]string) ([]byte, error) {
		return []byte(`{"services":{"db":{"image":"postgres:17-alpine@sha256:` + strings.Repeat("a", 64) + `"}}}`), nil
	}}
	got, err := (Runtime{f, "default"}).Resolve(context.Background(), "p", "f")
	if err != nil || got["db"] != "docker.io/library/postgres@sha256:"+strings.Repeat("a", 64) {
		t.Fatal(got, err)
	}
	f.run = func([]string) ([]byte, error) {
		return []byte(`{"services":{"db":{"image":"postgres:17-alpine"}}}`), nil
	}
	if _, err = (Runtime{f, "default"}).Resolve(context.Background(), "p", "f"); err == nil {
		t.Fatal("unpinned accepted")
	}
}
func TestCheckTargetAndCapabilities(t *testing.T) {
	endpoint := "unix:///var/run/docker.sock"
	f := &fakeRunner{run: func(a []string) ([]byte, error) {
		switch strings.Join(a, " ") {
		case "context inspect default":
			return []byte(`[{"Endpoints":{"docker":{"Host":"` + endpoint + `"}}}]`), nil
		case "--context default info --format {{.ID}}":
			return []byte("daemon"), nil
		default:
			return []byte("--wait-timeout --resolve-image-digests"), nil
		}
	}}
	r := Runtime{f, "default"}
	got, err := r.Check(context.Background())
	if err != nil || got.DaemonID != "daemon" {
		t.Fatal(got, err)
	}
	endpoint = "ssh://server"
	if _, err = r.Check(context.Background()); err == nil {
		t.Fatal("remote context accepted")
	}
}
func TestObserveOwnershipAndMalformed(t *testing.T) {
	owner := "ours"
	malformed := false
	f := &fakeRunner{run: func(a []string) ([]byte, error) {
		s := strings.Join(a, " ")
		if strings.Contains(s, "ps -aq") {
			return []byte("cid"), nil
		}
		if strings.Contains(s, "container inspect") {
			if malformed {
				return []byte("no json"), nil
			}
			return []byte(`[{"Id":"cid","Name":"/p-db-1","Image":"sha256:abc","Config":{"Labels":{"io.envctl.environment":"` + owner + `","com.docker.compose.project":"p","com.docker.compose.service":"db"}},"State":{"Status":"running","Health":{"Status":"healthy"}}}]`), nil
		}
		return nil, nil
	}}
	r := Runtime{f, "default"}
	got, err := r.Observe(context.Background(), "p", "ours")
	if err != nil || len(got) != 1 || got[0].Health != "healthy" {
		t.Fatal(got, err)
	}
	if got.Stable()[0].Health != "" || got[0].Health != "healthy" {
		t.Fatal("stable mutated snapshot")
	}
	owner = "foreign"
	if _, err = r.Observe(context.Background(), "p", "ours"); err == nil {
		t.Fatal("foreign ownership accepted")
	}
	owner = "ours"
	malformed = true
	if _, err = r.Observe(context.Background(), "p", "ours"); err == nil {
		t.Fatal("bad JSON accepted")
	}
}
func TestProcessAndCancellation(t *testing.T) {
	b, err := (Process{Binary: "/bin/echo"}).Run(context.Background(), "hello;not-shell")
	if err != nil || string(b) != "hello;not-shell\n" {
		t.Fatal(string(b), err)
	}
	if _, err = (Process{Binary: "/bin/false"}).Run(context.Background()); err == nil {
		t.Fatal("exit error hidden")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = (Process{Binary: "/bin/sleep"}).Run(ctx, "10"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestBufferAndEnvironment(t *testing.T) {
	b := &limitedBuffer{limit: 3}
	n, err := b.Write([]byte("abcdef"))
	if n != 6 || err != nil || b.String() != "abc" || !b.overflow {
		t.Fatal(b, n, err)
	}
	_, _ = b.Write([]byte("more"))
	t.Setenv("DOCKER_HOST", "bad")
	t.Setenv("COMPOSE_FILE", "bad")
	for _, e := range cleanEnv() {
		if e == "DOCKER_HOST=bad" || e == "COMPOSE_FILE=bad" {
			t.Fatal(e)
		}
	}
}
