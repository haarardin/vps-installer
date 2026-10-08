package spec

import (
	"reflect"
	"strings"
	"testing"
)

func valid() Spec {
	return Spec{APIVersion: APIVersion, Name: "dev", Services: []Service{{"db", "postgres", "17", 5432, true}, {"cache", "redis", "7", 6379, false}}}
}
func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Spec)
	}{
		{"api", func(s *Spec) { s.APIVersion = "v2" }}, {"path", func(s *Spec) { s.Name = "../bad" }}, {"shell", func(s *Spec) { s.Name = "x;id" }},
		{"empty", func(s *Spec) { s.Services = nil }}, {"many", func(s *Spec) { s.Services = make([]Service, 17) }},
		{"duplicate", func(s *Spec) { s.Services[1].Name = "db" }}, {"unknown", func(s *Spec) { s.Services[0].Template = "nginx" }},
		{"latest", func(s *Spec) { s.Services[0].Version = "latest" }}, {"negative", func(s *Spec) { s.Services[0].HostPort = -1 }},
		{"overflow", func(s *Spec) { s.Services[0].HostPort = 65536 }}, {"collision", func(s *Spec) { s.Services[1].HostPort = 5432 }},
		{"pg ephemeral", func(s *Spec) { s.Services[0].Persistent = false }},
	}
	if err := Validate(valid()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid()
			tc.change(&s)
			if Validate(s) == nil {
				t.Fatal("invalid spec accepted")
			}
		})
	}
	s := valid()
	s.Services[0].HostPort = 0
	s.Services[1].HostPort = 0
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
}
func TestDecodeStrict(t *testing.T) {
	b, err := Encode(valid())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(strings.NewReader(string(b)))
	if err != nil || !reflect.DeepEqual(got, Normalize(valid())) {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{string(b) + "unknown: true\n", string(b) + "name: duplicate\n", string(b) + "---\nname: other\n", strings.Repeat("x", MaxBytes+1), "name: &x foo\nservices: *x", `{"api_version":"envctl/v1alpha1","name":"dev","name":"duplicate"}`, ""} {
		if _, err = Decode(strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted malformed input: %.50s", bad)
		}
	}
}
func TestDiff(t *testing.T) {
	s := valid()
	changes, err := Diff(nil, s)
	if err != nil || len(changes) != 2 {
		t.Fatal(changes, err)
	}
	same, err := Diff(&s, s)
	if err != nil || len(same) != 0 {
		t.Fatal(same, err)
	}
	for _, change := range []func(*Spec){func(s *Spec) { s.Name = "other" }, func(s *Spec) { s.Services = s.Services[:1] }, func(s *Spec) { s.Services[1].Persistent = true }, func(s *Spec) { s.Services[0].Version = "18" }} {
		n := valid()
		change(&n)
		if _, err = Diff(&s, n); err == nil {
			t.Fatal("unsafe change allowed")
		}
	}
	n := valid()
	n.Services[0].HostPort = 15432
	changes, err = Diff(&s, n)
	if err != nil || len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatal(changes, err)
	}
	n = valid()
	n.Services = append(n.Services, Service{"cache2", "redis", "7", 0, false})
	changes, err = Diff(&s, n)
	if err != nil || len(changes) != 1 || changes[0].Kind != "create" {
		t.Fatal(changes, err)
	}
}
func TestNormalizeDoesNotMutateAndHashIsStable(t *testing.T) {
	s := valid()
	before := valid()
	norm := Normalize(s)
	if !reflect.DeepEqual(s, before) {
		t.Fatal("mutated")
	}
	s.Services[0], s.Services[1] = s.Services[1], s.Services[0]
	if Hash(norm) != Hash(Normalize(s)) {
		t.Fatal("unstable")
	}
	s.Services[0].HostPort = 10000
	if Hash(norm) == Hash(Normalize(s)) {
		t.Fatal("change missed")
	}
}
func FuzzDecode(f *testing.F) {
	b, _ := Encode(valid())
	f.Add(string(b))
	f.Add("a: &x [*x]")
	f.Fuzz(func(t *testing.T, s string) { _, _ = Decode(strings.NewReader(s)) })
}
