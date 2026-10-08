package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haarardin/vps-installer/internal/spec"
)

func TestLockAndIdentity(t *testing.T) {
	s := Store{Root: t.TempDir()}
	dir, unlock, err := s.Lock("dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Lock("dev"); err == nil {
		t.Fatal("concurrent lock acquired")
	}
	_, other, e := s.Lock("other")
	if e != nil {
		t.Fatal(e)
	}
	other()
	unlock()
	_, release, e := s.Lock("dev")
	if e != nil {
		t.Fatal(e)
	}
	release()
	if filepath.Base(dir) != "dev" {
		t.Fatal(dir)
	}
	if _, err = s.Dir("../escape"); err == nil {
		t.Fatal("traversal accepted")
	}
}
func TestAtomicAndStrictRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	want := struct {
		N int `json:"n"`
	}{3}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	var got struct {
		N int `json:"n"`
	}
	if err := Read(path, &got); err != nil || got != want {
		t.Fatal(got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	for _, bad := range []string{`{"n":1,"extra":2}`, `{"n":1} {}`, `broken`} {
		if err := Atomic(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Read(path, &got); err == nil {
			t.Fatal("corruption accepted")
		}
	}
	if _, err := ReadBytes(path, 1); err == nil {
		t.Fatal("size limit ignored")
	}
}
func TestSymlinksRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Root: link}).Dir("dev"); err == nil {
		t.Fatal("symlink dir accepted")
	}
	file := filepath.Join(dir, "file")
	if err := os.Symlink(filepath.Join(outside, "target"), file); err != nil {
		t.Fatal(err)
	}
	if err := Atomic(file, []byte("bad"), 0600); err == nil {
		t.Fatal("symlink target accepted")
	}
	if _, err := ReadBytes(file, 100); err == nil {
		t.Fatal("symlink read accepted")
	}
}
func TestSecretsStableAndMissingFails(t *testing.T) {
	dir := t.TempDir()
	s := spec.Spec{Services: []spec.Service{{Name: "db", Template: "postgres"}, {Name: "cache", Template: "redis"}}}
	if err := EnsureSecrets(dir, s, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secrets", "db.password")
	before, err := ReadBytes(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err = EnsureSecrets(dir, s, &s); err != nil {
		t.Fatal(err)
	}
	after, _ := ReadBytes(path, 128)
	if string(before) != string(after) {
		t.Fatal("secret rotated")
	}
	acl, err := ReadBytes(filepath.Join(dir, "secrets", "cache.acl"), 1024)
	if err != nil || !strings.Contains(string(acl), "user health on nopass -@all +ping") {
		t.Fatal(string(acl), err)
	}
	password, _ := ReadBytes(filepath.Join(dir, "secrets", "cache.password"), 128)
	if strings.Contains(string(acl), string(password)) {
		t.Fatal("raw password in ACL")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = EnsureSecrets(dir, s, &s); err == nil {
		t.Fatal("missing password regenerated")
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
