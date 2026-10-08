// Package spec defines the deliberately small, versioned public environment API.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"

	"go.yaml.in/yaml/v3"
)

const APIVersion = "envctl/v1alpha1"
const MaxBytes = 64 << 10

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func ValidName(s string) bool { return namePattern.MatchString(s) }

type Service struct {
	Name       string `json:"name" yaml:"name"`
	Template   string `json:"template" yaml:"template"`
	Version    string `json:"version" yaml:"version"`
	HostPort   int    `json:"host_port" yaml:"host_port"`
	Persistent bool   `json:"persistent" yaml:"persistent"`
}
type Spec struct {
	APIVersion string    `json:"api_version" yaml:"api_version"`
	Name       string    `json:"name" yaml:"name"`
	Services   []Service `json:"services" yaml:"services"`
}

// A small reviewed catalog. Major upgrades are deliberately not an MVP operation.
func Image(s Service) string { return "docker.io/library/" + s.Template + ":" + s.Version + "-alpine" }
func Validate(s Spec) error {
	if s.APIVersion != APIVersion {
		return errors.New("InvalidSpec: unsupported api_version")
	}
	if !ValidName(s.Name) {
		return errors.New("InvalidSpec: invalid environment name")
	}
	if len(s.Services) == 0 || len(s.Services) > 16 {
		return errors.New("InvalidSpec: expected 1..16 services")
	}
	names, ports := map[string]bool{}, map[int]bool{}
	for _, v := range s.Services {
		if !ValidName(v.Name) || names[v.Name] {
			return fmt.Errorf("InvalidSpec: invalid or duplicate service %q", v.Name)
		}
		names[v.Name] = true
		if !((v.Template == "postgres" && v.Version == "17") || (v.Template == "redis" && v.Version == "7")) {
			return errors.New("InvalidSpec: catalog supports postgres 17 and redis 7")
		}
		if v.HostPort < 0 || v.HostPort > 65535 {
			return errors.New("InvalidSpec: host_port must be 0..65535")
		}
		if v.HostPort != 0 {
			if ports[v.HostPort] {
				return errors.New("InvalidSpec: duplicate host_port")
			}
			ports[v.HostPort] = true
		}
		if v.Template == "postgres" && !v.Persistent {
			return errors.New("PolicyDenied: PostgreSQL requires persistent storage in MVP")
		}
	}
	return nil
}

func Decode(r io.Reader) (Spec, error) {
	var s Spec
	b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return s, err
	}
	if len(b) > MaxBytes {
		return s, errors.New("InvalidSpec: document too large")
	}
	var node yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err = dec.Decode(&node); err != nil {
		return s, fmt.Errorf("InvalidSpec: %w", err)
	}
	if err = checkNode(&node, 0); err != nil {
		return s, err
	}
	var extra yaml.Node
	if err = dec.Decode(&extra); err != io.EOF {
		return s, errors.New("InvalidSpec: expected one document")
	}
	strict := yaml.NewDecoder(bytes.NewReader(b))
	strict.KnownFields(true)
	if err = strict.Decode(&s); err != nil {
		return s, fmt.Errorf("InvalidSpec: %w", err)
	}
	if err = Validate(s); err != nil {
		return s, err
	}
	return Normalize(s), nil
}
func checkNode(n *yaml.Node, depth int) error {
	if depth > 12 || n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errors.New("InvalidSpec: aliases, anchors or excessive nesting")
	}
	if n.Kind == yaml.MappingNode {
		keys := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Value == "<<" || keys[k.Value] {
				return errors.New("InvalidSpec: duplicate or invalid key")
			}
			keys[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if err := checkNode(c, depth+1); err != nil {
			return err
		}
	}
	return nil
}
func Normalize(s Spec) Spec {
	s.Services = append([]Service(nil), s.Services...)
	sort.Slice(s.Services, func(i, j int) bool { return s.Services[i].Name < s.Services[j].Name })
	return s
}
func Encode(s Spec) ([]byte, error) { return yaml.Marshal(Normalize(s)) }
func Hash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("hash requires JSON-serializable domain types: " + err.Error())
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Change struct {
	Service string `json:"service"`
	Kind    string `json:"kind"`
	Details string `json:"details"`
}

func Diff(old *Spec, next Spec) ([]Change, error) {
	if err := Validate(next); err != nil {
		return nil, err
	}
	prior := map[string]Service{}
	if old != nil {
		if err := Validate(*old); err != nil {
			return nil, err
		}
		if old.Name != next.Name {
			return nil, errors.New("PolicyDenied: rename unsupported")
		}
		for _, s := range old.Services {
			prior[s.Name] = s
		}
	}
	changes := []Change{}
	for _, s := range Normalize(next).Services {
		p, ok := prior[s.Name]
		if !ok {
			changes = append(changes, Change{s.Name, "create", "create service"})
		} else {
			if p.Template != s.Template || p.Version != s.Version || p.Persistent != s.Persistent {
				return nil, errors.New("PolicyDenied: template, version and storage changes require a migration")
			}
			if p != s {
				changes = append(changes, Change{s.Name, "update", "container may be recreated; data retained"})
			}
		}
		delete(prior, s.Name)
	}
	if len(prior) > 0 {
		return nil, errors.New("PolicyDenied: service removal is not supported in MVP; down retains all data")
	}
	return changes, nil
}
