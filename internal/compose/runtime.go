package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type Target struct {
	Context  string `json:"context"`
	Endpoint string `json:"endpoint"`
	DaemonID string `json:"daemon_id"`
}
type ResourceState struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service,omitempty"`
	Image   string `json:"image,omitempty"`
	Config  string `json:"config,omitempty"`
	State   string `json:"state,omitempty"`
	Health  string `json:"health,omitempty"`
}
type Snapshot []ResourceState

// Fingerprint excludes transient health probe outcomes but detects container replacement/status changes.
func (s Snapshot) Stable() Snapshot {
	out := append(Snapshot{}, s...)
	for i := range out {
		out[i].Health = ""
	}
	return out
}

type Runtime struct {
	Runner  Runner
	Context string
}

func (r Runtime) run(ctx context.Context, args ...string) ([]byte, error) {
	return r.Runner.Run(ctx, append([]string{"--context", r.Context}, args...)...)
}
func (r Runtime) Check(ctx context.Context) (Target, error) {
	var t Target
	t.Context = r.Context
	b, err := r.Runner.Run(ctx, "context", "inspect", r.Context)
	if err != nil {
		return t, err
	}
	var contexts []struct {
		Endpoints struct{ Docker struct{ Host string } }
	}
	if err = json.Unmarshal(b, &contexts); err != nil || len(contexts) != 1 {
		return t, errors.New("invalid Docker context response")
	}
	t.Endpoint = contexts[0].Endpoints.Docker.Host
	if !strings.HasPrefix(t.Endpoint, "unix://") {
		return t, errors.New("PolicyDenied: MVP requires a local unix Docker socket; run envctl on the target host")
	}
	b, err = r.run(ctx, "info", "--format", "{{.ID}}")
	if err != nil {
		return t, err
	}
	t.DaemonID = strings.TrimSpace(string(b))
	if t.DaemonID == "" {
		return t, errors.New("Docker daemon ID is missing")
	}
	b, err = r.run(ctx, "compose", "up", "--help")
	if err != nil {
		return t, err
	}
	if !bytes.Contains(b, []byte("--wait-timeout")) {
		return t, errors.New("CapabilityMissing: Compose up --wait-timeout required")
	}
	b, err = r.run(ctx, "compose", "config", "--help")
	if err != nil {
		return t, err
	}
	if !bytes.Contains(b, []byte("--resolve-image-digests")) {
		return t, errors.New("CapabilityMissing: Compose digest resolution required")
	}
	return t, nil
}
func (r Runtime) command(ctx context.Context, project, file string, args ...string) ([]byte, error) {
	base := []string{"compose", "--env-file", "/dev/null", "--project-name", project, "--file", file}
	return r.run(ctx, append(base, args...)...)
}
func (r Runtime) Resolve(ctx context.Context, project, file string) (map[string]string, error) {
	b, err := r.command(ctx, project, file, "config", "--resolve-image-digests", "--format", "json")
	if err != nil {
		return nil, err
	}
	var f struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err = json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if len(f.Services) == 0 {
		return nil, errors.New("empty resolved Compose config")
	}
	images := map[string]string{}
	for name, s := range f.Services {
		parts := strings.Split(s.Image, "@sha256:")
		if len(parts) != 2 {
			return nil, errors.New("Compose did not pin an image digest")
		}
		repo := parts[0]
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		if !strings.Contains(repo, "/") {
			repo = "docker.io/library/" + repo
		} else if strings.HasPrefix(repo, "library/") {
			repo = "docker.io/" + repo
		}
		images[name] = repo + "@sha256:" + parts[1]
	}
	return images, nil
}
func (r Runtime) Validate(ctx context.Context, project, file string) error {
	_, err := r.command(ctx, project, file, "config", "--quiet")
	return err
}
func (r Runtime) Up(ctx context.Context, project, file string, seconds int) error {
	_, err := r.command(ctx, project, file, "up", "--wait", "--wait-timeout", fmt.Sprint(seconds), "--pull", "missing")
	return err
}
func (r Runtime) Down(ctx context.Context, project, file string) error {
	_, err := r.command(ctx, project, file, "down", "--timeout", "15")
	return err
}
func (r Runtime) Logs(ctx context.Context, project, file, service string) ([]byte, error) {
	return r.command(ctx, project, file, "logs", "--no-color", "--tail", "100", service)
}

func (r Runtime) Observe(ctx context.Context, project, owner string) (Snapshot, error) {
	result := Snapshot{}
	for _, kind := range []string{"container", "network", "volume"} {
		var args []string
		switch kind {
		case "container":
			args = []string{"ps", "-aq", "--filter", "label=com.docker.compose.project=" + project}
		case "network":
			args = []string{"network", "ls", "-q", "--filter", "name=" + project + "_"}
		case "volume":
			args = []string{"volume", "ls", "-q", "--filter", "name=" + project + "_"}
		}
		b, err := r.run(ctx, args...)
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(string(b))
		if len(ids) == 0 {
			continue
		}
		if len(ids) > 128 {
			return nil, errors.New("too many resources for environment")
		}
		b, err = r.run(ctx, append([]string{kind, "inspect"}, ids...)...)
		if err != nil {
			return nil, err
		}
		var objects []struct {
			ID     string `json:"Id"`
			Name   string
			Labels map[string]string
			Image  string
			Config struct{ Labels map[string]string }
			State  struct {
				Status string
				Health struct{ Status string }
			}
		}
		if err = json.Unmarshal(b, &objects); err != nil {
			return nil, fmt.Errorf("invalid Docker inspection: %w", err)
		}
		if len(objects) != len(ids) {
			return nil, errors.New("incomplete Docker inspection")
		}
		for _, o := range objects {
			labels := o.Labels
			if kind == "container" {
				labels = o.Config.Labels
			}
			if kind != "container" && !strings.HasPrefix(o.Name, project+"_") {
				continue
			}
			if labels[OwnerLabel] != owner || labels["com.docker.compose.project"] != project {
				return nil, fmt.Errorf("PolicyDenied: foreign %s resource in environment namespace", kind)
			}
			id := o.ID
			if kind == "volume" {
				id = o.Name
			}
			if id == "" || o.Name == "" {
				return nil, errors.New("incomplete resource identity")
			}
			result = append(result, ResourceState{Kind: kind, ID: id, Name: o.Name, Service: labels["com.docker.compose.service"], Image: o.Image, Config: labels["com.docker.compose.config-hash"], State: o.State.Status, Health: o.State.Health.Status})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return result, nil
}
