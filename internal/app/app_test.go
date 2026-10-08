package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haarardin/vps-installer/internal/compose"
	"github.com/haarardin/vps-installer/internal/spec"
	"github.com/haarardin/vps-installer/internal/state"
)

type fakeRuntime struct {
	target             compose.Target
	snapshot           compose.Snapshot
	upErr              error
	upCalls, downCalls int
	unhealthy          bool
	beforeUp           func()
	logs               []byte
}

func (f *fakeRuntime) Check(context.Context) (compose.Target, error) { return f.target, nil }
func (f *fakeRuntime) Observe(context.Context, string, string) (compose.Snapshot, error) {
	return f.snapshot, nil
}
func (f *fakeRuntime) Resolve(_ context.Context, _, file string) (map[string]string, error) {
	b, e := os.ReadFile(file)
	if e != nil {
		return nil, e
	}
	var c compose.File
	if e = json.Unmarshal(b, &c); e != nil {
		return nil, e
	}
	images := map[string]string{}
	for name, s := range c.Services {
		template := "redis"
		if strings.Contains(s.Image, "postgres") {
			template = "postgres"
		}
		images[name] = "docker.io/library/" + template + "@sha256:" + strings.Repeat("a", 64)
	}
	return images, nil
}
func (f *fakeRuntime) Validate(context.Context, string, string) error { return nil }
func (f *fakeRuntime) Up(_ context.Context, _, file string, _ int) error {
	f.upCalls++
	if f.beforeUp != nil {
		f.beforeUp()
	}
	if f.upErr != nil {
		return f.upErr
	}
	b, e := os.ReadFile(file)
	if e != nil {
		return e
	}
	var c compose.File
	if e = json.Unmarshal(b, &c); e != nil {
		return e
	}
	f.snapshot = compose.Snapshot{}
	for name := range c.Services {
		health := "healthy"
		if f.unhealthy {
			health = "unhealthy"
		}
		f.snapshot = append(f.snapshot, compose.ResourceState{Kind: "container", ID: name, Name: name, Service: name, State: "running", Health: health})
	}
	return nil
}
func (f *fakeRuntime) Down(context.Context, string, string) error {
	f.downCalls++
	f.snapshot = compose.Snapshot{}
	return nil
}
func (f *fakeRuntime) Logs(context.Context, string, string, string) ([]byte, error) {
	return f.logs, nil
}
func fixture() spec.Spec {
	return spec.Spec{APIVersion: spec.APIVersion, Name: "dev", Services: []spec.Service{{Name: "db", Template: "postgres", Version: "17", Persistent: true}}}
}
func setup(t *testing.T) (App, *fakeRuntime) {
	t.Helper()
	r := &fakeRuntime{target: compose.Target{Context: "default", Endpoint: "unix:///socket", DaemonID: "daemon"}, snapshot: compose.Snapshot{}}
	return App{Store: state.Store{Root: t.TempDir()}, Runtime: r}, r
}
func TestLifecycleAndPersistentSecrets(t *testing.T) {
	a, r := setup(t)
	ctx := context.Background()
	p, err := a.Plan(ctx, fixture())
	if err != nil {
		t.Fatal(err)
	}
	if r.upCalls != 0 {
		t.Fatal("plan mutated runtime")
	}
	if err = a.Apply(ctx, a.PlanPath(p)); err != nil {
		t.Fatal(err)
	}
	dir, _ := a.Store.Dir("dev")
	password, err := state.ReadBytes(filepath.Join(dir, "secrets", "db.password"), 128)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Apply(ctx, a.PlanPath(p)); err == nil {
		t.Fatal("old plan replayed")
	}
	status, err := a.Status(ctx, "dev")
	if err != nil || status.Record.Phase != "ready" || status.Record.LastSuccessful == nil {
		t.Fatal(status, err)
	}
	r.logs = []byte("password=" + string(password))
	logs, err := a.Logs(ctx, "dev", "db")
	if err != nil || strings.Contains(string(logs), string(password)) {
		t.Fatal(string(logs), err)
	}
	down, err := a.PlanDown(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Apply(ctx, a.PlanPath(down)); err != nil {
		t.Fatal(err)
	}
	if r.downCalls != 1 {
		t.Fatal(r.downCalls)
	}
	after, err := state.ReadBytes(filepath.Join(dir, "secrets", "db.password"), 128)
	if err != nil || string(after) != string(password) {
		t.Fatal("secret lost")
	}
	again, err := a.Plan(ctx, fixture())
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Apply(ctx, a.PlanPath(again)); err != nil {
		t.Fatal(err)
	}
}
func TestStaleTargetAndObservationPreventEffects(t *testing.T) {
	for _, kind := range []string{"target", "observation", "fingerprint", "spec"} {
		t.Run(kind, func(t *testing.T) {
			a, r := setup(t)
			p, err := a.Plan(context.Background(), fixture())
			if err != nil {
				t.Fatal(err)
			}
			path := a.PlanPath(p)
			switch kind {
			case "target":
				r.target.DaemonID = "other"
			case "observation":
				r.snapshot = compose.Snapshot{{Kind: "container", ID: "changed"}}
			case "fingerprint":
				p.Fingerprint = "tampered"
				if err = state.Write(path, p); err != nil {
					t.Fatal(err)
				}
			case "spec":
				p.Spec.Services[0].Persistent = false
				p.Fingerprint = p.hash()
				if err = state.Write(path, p); err != nil {
					t.Fatal(err)
				}
			}
			if err = a.Apply(context.Background(), path); err == nil {
				t.Fatal("stale plan applied")
			}
			if r.upCalls != 0 {
				t.Fatal("effect occurred")
			}
		})
	}
}
func TestFailureRetainsSecretsAndCanReplan(t *testing.T) {
	a, r := setup(t)
	ctx := context.Background()
	p, err := a.Plan(ctx, fixture())
	if err != nil {
		t.Fatal(err)
	}
	r.upErr = errors.New("port conflict")
	if err = a.Apply(ctx, a.PlanPath(p)); err == nil {
		t.Fatal("error hidden")
	}
	status, err := a.Status(ctx, "dev")
	if err != nil || status.Record.Phase != "failed" || status.Record.LastSuccessful != nil {
		t.Fatal(status, err)
	}
	if r.downCalls != 0 {
		t.Fatal("automatic cleanup")
	}
	r.upErr = nil
	p, err = a.Plan(ctx, fixture())
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Apply(ctx, a.PlanPath(p)); err != nil {
		t.Fatal(err)
	}
}
func TestHealthAndJournalBeforeEffect(t *testing.T) {
	a, r := setup(t)
	ctx := context.Background()
	p, err := a.Plan(ctx, fixture())
	if err != nil {
		t.Fatal(err)
	}
	r.unhealthy = true
	r.beforeUp = func() {
		dir, _ := a.Store.Dir("dev")
		record, e := load(dir, "dev", false)
		if e != nil || record.Phase != "applying" {
			t.Fatal(record, e)
		}
		var op Operation
		if e = state.Read(filepath.Join(dir, "operations", record.Operation+".json"), &op); e != nil || op.Phase != "applying" {
			t.Fatal(op, e)
		}
	}
	if err = a.Apply(ctx, a.PlanPath(p)); err == nil || !strings.Contains(err.Error(), "ReadinessFailed") {
		t.Fatal(err)
	}
}
func TestManifestTamperingIsNotExecuted(t *testing.T) {
	a, _ := setup(t)
	ctx := context.Background()
	p, err := a.Plan(ctx, fixture())
	if err != nil {
		t.Fatal(err)
	}
	planManifest := filepath.Join(filepath.Dir(a.PlanPath(p)), "compose.json")
	if err = os.WriteFile(planManifest, []byte(`{"services":{"evil":{"privileged":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Apply(ctx, a.PlanPath(p)); err != nil {
		t.Fatal(err)
	}
	dir, _ := a.Store.Dir("dev")
	b, _ := os.ReadFile(filepath.Join(dir, "compose.json"))
	if strings.Contains(string(b), "privileged") {
		t.Fatal("executed mutable artifact")
	}
}
func TestStateWriteFailureAfterRuntimeSuccess(t *testing.T) {
	a, r := setup(t)
	ctx := context.Background()
	p, err := a.Plan(ctx, fixture())
	if err != nil {
		t.Fatal(err)
	}
	r.beforeUp = func() {
		dir, _ := a.Store.Dir("dev")
		path := filepath.Join(dir, "state.json")
		if err := os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path+".saved", path); err != nil {
			t.Fatal(err)
		}
	}
	err = a.Apply(ctx, a.PlanPath(p))
	if err == nil || !strings.Contains(err.Error(), "StateWriteFailed") {
		t.Fatal(err)
	}
	if r.downCalls != 0 {
		t.Fatal("data cleanup on persistence failure")
	}
}
