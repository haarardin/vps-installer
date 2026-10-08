//go:build linux

// Package state owns durable local files. One trusted Unix user owns this state root.
package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/haarardin/vps-installer/internal/spec"
)

type Store struct{ Root string }

func DefaultRoot() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "envctl")
}
func ID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func secureDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Reject symlink ancestors; state files are not intended to be shared across users.
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, "/"), "/") {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(cur, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(cur)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe state directory")
		}
	}
	return os.Chmod(abs, 0700)
}
func (s Store) Dir(name string) (string, error) {
	if !spec.ValidName(name) {
		return "", errors.New("invalid environment name")
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(root, "$\n\r") {
		return "", errors.New("state path contains unsupported interpolation characters")
	}
	if err = secureDir(root); err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if err = secureDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}
func (s Store) Lock(name string) (string, func(), error) {
	dir, err := s.Dir(name)
	if err != nil {
		return "", nil, err
	}
	fd, err := syscall.Open(filepath.Join(dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return "", nil, err
	}
	f := os.NewFile(uintptr(fd), "lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return "", nil, errors.New("Busy: another operation holds the environment lock")
	}
	return dir, func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
}
func Read(path string, v any) error {
	b, err := ReadBytes(path, 2<<20)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		return err
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing state data")
	}
	return nil
}
func ReadBytes(path string, limit int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("expected regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file too large")
	}
	return b, nil
}
func Write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return Atomic(path, b, 0600)
}
func Atomic(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := secureDir(dir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("unsafe destination")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func EnsureSecrets(dir string, s spec.Spec, previous *spec.Spec) error {
	existing := map[string]bool{}
	if previous != nil {
		for _, v := range previous.Services {
			existing[v.Name] = true
		}
	}
	if err := secureDir(filepath.Join(dir, "secrets")); err != nil {
		return err
	}
	for _, v := range s.Services {
		path := filepath.Join(dir, "secrets", v.Name+".password")
		b, err := ReadBytes(path, 128)
		if errors.Is(err, os.ErrNotExist) {
			if existing[v.Name] {
				return fmt.Errorf("secret missing for existing service %s; restore it rather than rotate credentials", v.Name)
			}
			raw := make([]byte, 32)
			if _, err = rand.Read(raw); err != nil {
				return err
			}
			b = []byte(hex.EncodeToString(raw))
			if err = Atomic(path, b, 0600); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if len(b) != 64 {
			return errors.New("invalid stored secret")
		}
		if _, err = hex.DecodeString(string(b)); err != nil {
			return errors.New("invalid stored secret")
		}
		// Parent directory remains 0700; mounted files must be readable by image-specific service UIDs.
		if v.Template == "postgres" {
			if err = os.Chmod(path, 0444); err != nil {
				return err
			}
		}
		if v.Template == "redis" {
			sum := sha256.Sum256(b)
			acl := fmt.Sprintf("user default on #%x ~* &* +@all\nuser health on nopass -@all +ping\n", sum)
			if err = Atomic(filepath.Join(dir, "secrets", v.Name+".acl"), []byte(acl), 0444); err != nil {
				return err
			}
		}
	}
	return nil
}

// SecretsHash detects changes to managed credential files without returning their values.
func SecretsHash(dir string, s *spec.Spec) (string, error) {
	if s == nil {
		return spec.Hash(map[string]string{}), nil
	}
	values := map[string]string{}
	for _, v := range s.Services {
		files := []string{v.Name + ".password"}
		if v.Template == "redis" {
			files = append(files, v.Name+".acl")
		}
		for _, name := range files {
			b, err := ReadBytes(filepath.Join(dir, "secrets", name), 1024)
			if err != nil {
				return "", fmt.Errorf("managed credentials unavailable for %s", v.Name)
			}
			values[name] = spec.Hash(string(b))
		}
	}
	return spec.Hash(values), nil
}
