// Package app coordinates durable operations; Runtime is replaceable in tests.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/haarardin/vps-installer/internal/compose"
	"github.com/haarardin/vps-installer/internal/spec"
	"github.com/haarardin/vps-installer/internal/state"
)

type Runtime interface {
	Check(context.Context) (compose.Target, error)
	Observe(context.Context, string, string) (compose.Snapshot, error)
	Resolve(context.Context, string, string) (map[string]string, error)
	Validate(context.Context, string, string) error
	Up(context.Context, string, string, int) error
	Down(context.Context, string, string) error
	Logs(context.Context, string, string, string) ([]byte, error)
}
type App struct {
	Store       state.Store
	Runtime     Runtime
	WaitSeconds int
}
type Record struct {
	SecretsHash    string            `json:"secrets_hash,omitempty"`
	Version        int               `json:"version"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Revision       uint64            `json:"revision"`
	Target         *compose.Target   `json:"target,omitempty"`
	Managed        *spec.Spec        `json:"managed,omitempty"`
	LastSuccessful *spec.Spec        `json:"last_successful,omitempty"`
	Images         map[string]string `json:"images,omitempty"`
	Phase          string            `json:"phase"`
	Operation      string            `json:"operation,omitempty"`
}
type Plan struct {
	SecretsHash     string            `json:"secrets_hash"`
	Version         int               `json:"version"`
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	EnvironmentID   string            `json:"environment_id"`
	Action          string            `json:"action"`
	BaseRevision    uint64            `json:"base_revision"`
	Target          compose.Target    `json:"target"`
	Spec            spec.Spec         `json:"spec"`
	Images          map[string]string `json:"images"`
	ObservationHash string            `json:"observation_hash"`
	ManifestHash    string            `json:"manifest_hash"`
	Renderer        string            `json:"renderer"`
	Changes         []spec.Change     `json:"changes"`
	Fingerprint     string            `json:"fingerprint"`
}
type Operation struct {
	ID        string    `json:"id"`
	PlanID    string    `json:"plan_id"`
	Phase     string    `json:"phase"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (p Plan) hash() string    { p.Fingerprint = ""; return spec.Hash(p) }
func Project(id string) string { return "envctl-" + id }
func load(dir, name string, create bool) (Record, error) {
	var r Record
	err := state.Read(filepath.Join(dir, "state.json"), &r)
	if errors.Is(err, os.ErrNotExist) && create {
		id, e := state.ID()
		if e != nil {
			return r, e
		}
		r = Record{Version: 1, ID: id, Name: name, Phase: "draft"}
		return r, state.Write(filepath.Join(dir, "state.json"), r)
	}
	if err != nil {
		return r, err
	}
	if r.Version != 1 || r.Name != name || len(r.ID) != 32 {
		return r, errors.New("invalid state identity or version")
	}
	return r, nil
}
func sameTarget(r Record, t compose.Target) error {
	if r.Target != nil && *r.Target != t {
		return errors.New("TargetMismatch: environment belongs to another Docker target")
	}
	return nil
}
func (a App) Plan(ctx context.Context, s spec.Spec) (Plan, error) {
	if err := spec.Validate(s); err != nil {
		return Plan{}, err
	}
	return a.makePlan(ctx, s.Name, s, "up")
}
func (a App) PlanDown(ctx context.Context, name string) (Plan, error) {
	return a.makePlan(ctx, name, spec.Spec{}, "down")
}
func (a App) makePlan(ctx context.Context, name string, s spec.Spec, action string) (Plan, error) {
	var p Plan
	dir, unlock, err := a.Store.Lock(name)
	if err != nil {
		return p, err
	}
	defer unlock()
	r, err := load(dir, name, action == "up")
	if err != nil {
		return p, err
	}
	if action == "down" {
		if r.Managed == nil {
			return p, errors.New("environment has never been applied")
		}
		s = *r.Managed
	}
	s = spec.Normalize(s)
	credentialHash, err := state.SecretsHash(dir, r.Managed)
	if err != nil {
		return p, err
	}
	if r.Managed != nil && credentialHash != r.SecretsHash {
		return p, errors.New("managed credentials changed; restore original files")
	}
	changes, err := spec.Diff(r.Managed, s)
	if err != nil {
		return p, err
	}
	t, err := a.Runtime.Check(ctx)
	if err != nil {
		return p, err
	}
	if err = sameTarget(r, t); err != nil {
		return p, err
	}
	observed, err := a.Runtime.Observe(ctx, Project(r.ID), r.ID)
	if err != nil {
		return p, err
	}
	id, err := state.ID()
	if err != nil {
		return p, err
	}
	planDir := filepath.Join(dir, "plans", id)
	raw, err := compose.Render(s, r.ID, dir, nil)
	if err != nil {
		return p, err
	}
	// Keep already managed images pinned when adding or reconfiguring services.
	var f compose.File
	if err = json.Unmarshal(raw, &f); err != nil {
		return p, err
	}
	for key, img := range r.Images {
		c, ok := f.Services[key]
		if ok {
			c.Image = img
			f.Services[key] = c
		}
	}
	raw, err = json.MarshalIndent(f, "", "  ")
	if err != nil {
		return p, err
	}
	path := filepath.Join(planDir, "compose.json")
	if err = state.Atomic(path, raw, 0600); err != nil {
		return p, err
	}
	images := r.Images
	if action == "up" {
		images, err = a.Runtime.Resolve(ctx, Project(r.ID), path)
		if err != nil {
			return p, err
		}
	}
	raw, err = compose.Render(s, r.ID, dir, images)
	if err != nil {
		return p, err
	}
	if err = state.Atomic(path, raw, 0600); err != nil {
		return p, err
	}
	if err = a.Runtime.Validate(ctx, Project(r.ID), path); err != nil {
		return p, err
	}
	if action == "down" {
		changes = []spec.Change{{Service: "*", Kind: "stop", Details: "remove containers/network; retain volumes and credentials"}}
	} else if len(changes) == 0 {
		changes = []spec.Change{{Service: "*", Kind: "reconcile", Details: "ensure services are running and healthy; retain data"}}
	}
	p = Plan{SecretsHash: credentialHash, Version: 1, ID: id, Name: name, EnvironmentID: r.ID, Action: action, BaseRevision: r.Revision, Target: t, Spec: s, Images: images, ObservationHash: spec.Hash(observed.Stable()), ManifestHash: spec.Hash(string(raw)), Renderer: compose.RendererVersion, Changes: changes}
	p.Fingerprint = p.hash()
	if err = state.Write(filepath.Join(planDir, "plan.json"), p); err != nil {
		return p, err
	}
	return p, nil
}
func (a App) PlanPath(p Plan) string {
	root, _ := filepath.Abs(a.Store.Root)
	return filepath.Join(root, p.Name, "plans", p.ID, "plan.json")
}
func (a App) Apply(ctx context.Context, path string) error {
	var p Plan
	if err := state.Read(path, &p); err != nil {
		return err
	}
	if p.Version != 1 || p.Renderer != compose.RendererVersion || p.Fingerprint != p.hash() {
		return errors.New("PlanStale: invalid plan fingerprint/version")
	}
	if p.Action != "up" && p.Action != "down" {
		return errors.New("invalid action")
	}
	if p.Spec.Name != p.Name {
		return errors.New("invalid spec identity")
	}
	if err := spec.Validate(p.Spec); err != nil {
		return err
	}
	dir, unlock, err := a.Store.Lock(p.Name)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := load(dir, p.Name, false)
	if err != nil {
		return err
	}
	credentialHash, err := state.SecretsHash(dir, r.Managed)
	if err != nil {
		return err
	}
	if credentialHash != p.SecretsHash || (r.Managed != nil && credentialHash != r.SecretsHash) {
		return errors.New("PlanStale: managed credentials changed")
	}
	if r.ID != p.EnvironmentID || r.Revision != p.BaseRevision {
		return errors.New("PlanStale: state revision changed")
	}
	if _, err = spec.Diff(r.Managed, p.Spec); err != nil {
		return err
	}
	if p.Action == "down" && (r.Managed == nil || spec.Hash(*r.Managed) != spec.Hash(p.Spec)) {
		return errors.New("PlanStale: down must use managed spec")
	}
	t, err := a.Runtime.Check(ctx)
	if err != nil {
		return err
	}
	if t != p.Target {
		return errors.New("TargetMismatch: target changed since plan")
	}
	if err = sameTarget(r, t); err != nil {
		return err
	}
	observed, err := a.Runtime.Observe(ctx, Project(r.ID), r.ID)
	if err != nil {
		return err
	}
	if spec.Hash(observed.Stable()) != p.ObservationHash {
		return errors.New("PlanStale: Docker resources changed; create a new plan")
	}
	raw, err := compose.Render(p.Spec, r.ID, dir, p.Images)
	if err != nil {
		return err
	}
	if spec.Hash(string(raw)) != p.ManifestHash {
		return errors.New("PlanStale: renderer or location changed")
	}
	// Never execute a user-edited manifest: regenerate from the validated model.
	manifest := filepath.Join(dir, "compose.json")
	if err = state.Atomic(manifest, raw, 0600); err != nil {
		return err
	}
	opID, err := state.ID()
	if err != nil {
		return err
	}
	opPath := filepath.Join(dir, "operations", opID+".json")
	op := Operation{ID: opID, PlanID: p.ID, Phase: "applying", UpdatedAt: time.Now().UTC()}
	if err = state.Write(opPath, op); err != nil {
		return err
	}
	previous := r.Managed
	r.Revision++
	r.Phase = "applying"
	r.Operation = opID
	r.Target = &t
	if err = state.Write(filepath.Join(dir, "state.json"), r); err != nil {
		return err
	}
	perform := func() error {
		if p.Action == "up" {
			if err := state.EnsureSecrets(dir, p.Spec, previous); err != nil {
				return err
			}
			// Only mark services as managed after their reusable credentials exist.
			// A crash during first-time secret preparation can then be retried.
			r.SecretsHash, err = state.SecretsHash(dir, &p.Spec)
			if err != nil {
				return err
			}
			r.Managed = &p.Spec
			r.Images = p.Images
			if err = state.Write(filepath.Join(dir, "state.json"), r); err != nil {
				return err
			}
			if err := a.Runtime.Validate(ctx, Project(r.ID), manifest); err != nil {
				return err
			}
			seconds := a.WaitSeconds
			if seconds <= 0 {
				seconds = 120
			}
			if err := a.Runtime.Up(ctx, Project(r.ID), manifest, seconds); err != nil {
				return err
			}
			now, err := a.Runtime.Observe(ctx, Project(r.ID), r.ID)
			if err != nil {
				return err
			}
			for _, s := range p.Spec.Services {
				count := 0
				for _, c := range now {
					if c.Kind == "container" && c.Service == s.Name {
						count++
						if c.State != "running" || c.Health != "healthy" {
							return fmt.Errorf("ReadinessFailed: %s is not healthy", s.Name)
						}
					}
				}
				if count != 1 {
					return fmt.Errorf("ReadinessFailed: expected one container for %s", s.Name)
				}
			}
			return nil
		}
		if err := a.Runtime.Down(ctx, Project(r.ID), manifest); err != nil {
			return err
		}
		remaining, err := a.Runtime.Observe(ctx, Project(r.ID), r.ID)
		if err != nil {
			return err
		}
		for _, resource := range remaining {
			if resource.Kind != "volume" {
				return errors.New("DownIncomplete: runtime resources remain")
			}
		}
		return nil
	}
	effectErr := perform()
	if effectErr != nil {
		r.Phase = "failed"
		if ctx.Err() != nil {
			r.Phase = "interrupted"
		}
	} else if p.Action == "down" {
		r.Phase = "stopped"
	} else {
		r.Phase = "ready"
		r.LastSuccessful = &p.Spec
	}
	r.Revision++
	// A persistence failure leaves the previous applying record for reconciliation.
	if err = state.Write(filepath.Join(dir, "state.json"), r); err != nil {
		return errors.Join(effectErr, fmt.Errorf("StateWriteFailed: runtime outcome requires reconciliation: %w", err))
	}
	op.Phase = r.Phase
	op.UpdatedAt = time.Now().UTC()
	if err = state.Write(opPath, op); err != nil {
		return errors.Join(effectErr, err)
	}
	return effectErr
}

type Status struct {
	Record    Record           `json:"record"`
	Resources compose.Snapshot `json:"resources"`
}

func (a App) Status(ctx context.Context, name string) (Status, error) {
	var out Status
	dir, unlock, err := a.Store.Lock(name)
	if err != nil {
		return out, err
	}
	defer unlock()
	r, err := load(dir, name, false)
	if err != nil {
		return out, err
	}
	t, err := a.Runtime.Check(ctx)
	if err != nil {
		return out, err
	}
	if err = sameTarget(r, t); err != nil {
		return out, err
	}
	observed, err := a.Runtime.Observe(ctx, Project(r.ID), r.ID)
	return Status{r, observed}, err
}
func (a App) Logs(ctx context.Context, name, service string) ([]byte, error) {
	dir, unlock, err := a.Store.Lock(name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := load(dir, name, false)
	if err != nil {
		return nil, err
	}
	if r.Managed == nil {
		return nil, errors.New("no managed services")
	}
	found := false
	for _, s := range r.Managed.Services {
		if s.Name == service {
			found = true
		}
	}
	if !found {
		return nil, errors.New("unknown service")
	}
	t, err := a.Runtime.Check(ctx)
	if err != nil {
		return nil, err
	}
	if err = sameTarget(r, t); err != nil {
		return nil, err
	}
	if _, err = a.Runtime.Observe(ctx, Project(r.ID), r.ID); err != nil {
		return nil, err
	}
	// Render rather than trusting an editable file on disk.
	b, err := compose.Render(*r.Managed, r.ID, dir, r.Images)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "compose.json")
	if err = state.Atomic(path, b, 0600); err != nil {
		return nil, err
	}
	b, err = a.Runtime.Logs(ctx, Project(r.ID), path, service)
	if err != nil {
		return nil, err
	}
	for _, s := range r.Managed.Services {
		secret, e := state.ReadBytes(filepath.Join(dir, "secrets", s.Name+".password"), 128)
		if e != nil {
			return nil, errors.New("cannot redact logs: secret unavailable")
		}
		b = bytesReplace(b, secret)
	}
	return b, nil
}
