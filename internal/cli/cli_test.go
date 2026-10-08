package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfflineWorkflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environment.yaml")
	var out, errOut bytes.Buffer
	for _, args := range [][]string{{"help"}, {"init", "--name", "demo", "--publish", "--output", path}, {"validate", "--file", path}} {
		if err := Run(context.Background(), args, &out, &errOut, nil); err != nil {
			t.Fatal(args, err)
		}
	}
	if !strings.Contains(out.String(), `"valid": true`) {
		t.Fatal(out.String())
	}
	before, _ := os.ReadFile(path)
	if err := Run(context.Background(), []string{"init", "--output", path}, &out, &errOut, nil); err == nil {
		t.Fatal("overwrote file")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("file modified")
	}
}
func TestArgumentErrors(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"apply"}, {"status"}, {"logs", "name"}, {"down"}, {"connections"}, {"create"}, {"--timeout", "0s", "help", "extra"}, {"init", "unexpected"}, {"doctor", "extra"}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out, &out, nil); err == nil && args[0] != "--timeout" {
			t.Fatalf("accepted %v", args)
		}
	}
}
