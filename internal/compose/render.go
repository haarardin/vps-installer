package compose

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/haarardin/vps-installer/internal/spec"
)

const OwnerLabel = "io.envctl.environment"
const RendererVersion = "1"

var digestPattern = regexp.MustCompile(`^docker\.io/library/(postgres|redis)@sha256:[a-f0-9]{64}$`)

func ValidImage(s spec.Service, image string) bool {
	return digestPattern.MatchString(image) && len(image) > len("docker.io/library/"+s.Template+"@") && image[:len("docker.io/library/"+s.Template+"@")] == "docker.io/library/"+s.Template+"@"
}

type File struct {
	Services map[string]Service  `json:"services"`
	Volumes  map[string]Resource `json:"volumes,omitempty"`
	Networks map[string]Resource `json:"networks"`
	Secrets  map[string]Secret   `json:"secrets,omitempty"`
}
type Resource struct {
	Labels map[string]string `json:"labels"`
}
type Secret struct {
	File string `json:"file"`
}
type Service struct {
	Image       string            `json:"image"`
	Labels      map[string]string `json:"labels"`
	Environment map[string]string `json:"environment,omitempty"`
	Ports       []string          `json:"ports,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Secrets     []string          `json:"secrets,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Healthcheck Healthcheck       `json:"healthcheck"`
	Restart     string            `json:"restart"`
	MemLimit    string            `json:"mem_limit"`
	CPUs        float64           `json:"cpus"`
	Logging     Logging           `json:"logging"`
	Tmpfs       []string          `json:"tmpfs,omitempty"`
}
type Logging struct {
	Driver  string            `json:"driver"`
	Options map[string]string `json:"options"`
}
type Healthcheck struct {
	Test        []string `json:"test"`
	Interval    string   `json:"interval"`
	Timeout     string   `json:"timeout"`
	Retries     int      `json:"retries"`
	StartPeriod string   `json:"start_period"`
}

// Render emits JSON, a Compose-supported YAML subset. Only trusted templates add commands.
func Render(s spec.Spec, id, dir string, images map[string]string) ([]byte, error) {
	if err := spec.Validate(s); err != nil {
		return nil, err
	}
	labels := map[string]string{OwnerLabel: id}
	f := File{Services: map[string]Service{}, Volumes: map[string]Resource{}, Networks: map[string]Resource{"default": {Labels: labels}}, Secrets: map[string]Secret{}}
	for _, v := range s.Services {
		img := spec.Image(v)
		if images != nil {
			var ok bool
			img, ok = images[v.Name]
			if !ok || !ValidImage(v, img) {
				return nil, fmt.Errorf("invalid pinned image for %s", v.Name)
			}
		}
		c := Service{Image: img, Labels: labels, Restart: "unless-stopped", MemLimit: "512m", CPUs: 1, Logging: Logging{"json-file", map[string]string{"max-size": "10m", "max-file": "3"}}, Healthcheck: Healthcheck{Interval: "2s", Timeout: "3s", Retries: 30, StartPeriod: "10s"}}
		secretName := v.Name + "-password"
		if v.Template == "postgres" {
			f.Secrets[secretName] = Secret{filepath.Join(dir, "secrets", v.Name+".password")}
			c.Secrets = []string{secretName}
			c.Environment = map[string]string{"POSTGRES_USER": "app", "POSTGRES_DB": "app", "POSTGRES_PASSWORD_FILE": "/run/secrets/" + secretName}
			// pg_isready alone can see the temporary initialization server. Use a TCP check instead.
			c.Healthcheck.Test = []string{"CMD", "pg_isready", "-h", "127.0.0.1", "-U", "app", "-d", "app"}
		} else {
			// Redis ACL contains only a SHA-256 password hash; raw password remains host-side.
			acl := v.Name + "-acl"
			f.Secrets[acl] = Secret{filepath.Join(dir, "secrets", v.Name+".acl")}
			c.Secrets = []string{acl}
			c.Command = []string{"redis-server", "--aclfile", "/run/secrets/" + acl, "--save", "", "--appendonly", "no"}
			if v.Persistent {
				c.Command = []string{"redis-server", "--aclfile", "/run/secrets/" + acl, "--appendonly", "yes"}
			}
			c.Healthcheck.Test = []string{"CMD-SHELL", "test \"$$(redis-cli --user health --pass '' --no-auth-warning ping)\" = PONG"}
		}
		port, path := 5432, "/var/lib/postgresql/data"
		if v.Template == "redis" {
			port = 6379
			path = "/data"
		}
		if v.HostPort > 0 {
			c.Ports = []string{fmt.Sprintf("127.0.0.1:%d:%d", v.HostPort, port)}
		}
		if v.Persistent {
			key := v.Name + "-data"
			f.Volumes[key] = Resource{Labels: labels}
			c.Volumes = []string{key + ":" + path}
		} else {
			c.Tmpfs = []string{"/data:rw,noexec,nosuid,size=134217728"}
		}
		f.Services[v.Name] = c
	}
	return json.MarshalIndent(f, "", "  ")
}
