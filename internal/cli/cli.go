// Package cli exposes a testable command dispatcher without global flags or os.Exit.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/haarardin/vps-installer/internal/app"
	"github.com/haarardin/vps-installer/internal/compose"
	"github.com/haarardin/vps-installer/internal/intent"
	"github.com/haarardin/vps-installer/internal/spec"
	"github.com/haarardin/vps-installer/internal/state"
)

const Help = `envctl — prompt-assisted Docker development environments (Linux)

Global options precede the command:
  --state-dir PATH     private state root (default XDG state directory)
  --context NAME       local Docker context (default: default)
  --timeout DURATION   whole-command timeout (default: 5m)

Commands:
  init --name NAME [--publish] [--output environment.yaml]
  create [--output environment.yaml] "description"
  validate --file environment.yaml
  plan --file environment.yaml
  apply /absolute/path/to/plan.json
  up --file environment.yaml [--yes]
  down [--yes] NAME
  status NAME
  logs NAME SERVICE
  connections [--show-secrets] NAME
  doctor

plan/status/connections output JSON. up/down only apply with --yes;
otherwise they save and show a plan. apply explicitly authorizes that plan.
No command deletes persistent data. create uses ENVCTL_AI_ENDPOINT (full
chat-completions URL), ENVCTL_AI_MODEL, optional ENVCTL_AI_API_KEY.
`

type Factory func(state.Store, string) app.App

func Run(ctx context.Context, args []string, out, errOut io.Writer, factory Factory) error {
	root := flag.NewFlagSet("envctl", flag.ContinueOnError)
	root.SetOutput(errOut)
	stateDir := root.String("state-dir", state.DefaultRoot(), "state directory")
	dockerContext := root.String("context", "default", "local Docker context")
	timeout := root.Duration("timeout", 5*time.Minute, "command deadline")
	root.Usage = func() { fmt.Fprint(out, Help) }
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = root.Args()
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(out, Help)
		return nil
	}
	if *timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	st := state.Store{Root: *stateDir}
	var application app.App
	if factory != nil {
		application = factory(st, *dockerContext)
	} else {
		application = app.App{Store: st, Runtime: compose.Runtime{Runner: compose.Process{}, Context: *dockerContext}, WaitSeconds: 120}
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	printJSON := func(v any) error { enc := json.NewEncoder(out); enc.SetIndent("", "  "); return enc.Encode(v) }
	showPlan := func(p app.Plan) error {
		return printJSON(struct {
			Plan app.Plan `json:"plan"`
			Path string   `json:"path"`
		}{p, application.PlanPath(p)})
	}
	switch command {
	case "init", "create":
		output := fs.String("output", "environment.yaml", "output path")
		name := fs.String("name", "backend-dev", "environment name (init)")
		publish := fs.Bool("publish", false, "publish on loopback (init)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var s spec.Spec
		if command == "init" {
			if fs.NArg() != 0 {
				return errors.New("init takes no positional arguments")
			}
			s = spec.Spec{APIVersion: spec.APIVersion, Name: *name, Services: []spec.Service{{Name: "database", Template: "postgres", Version: "17", Persistent: true}, {Name: "cache", Template: "redis", Version: "7"}}}
			if *publish {
				s.Services[0].HostPort = 5432
				s.Services[1].HostPort = 6379
			}
		} else {
			if fs.NArg() != 1 {
				return errors.New("create requires one quoted description")
			}
			client := intent.Client{HTTP: &http.Client{Timeout: 60 * time.Second}, Endpoint: os.Getenv("ENVCTL_AI_ENDPOINT"), Model: os.Getenv("ENVCTL_AI_MODEL"), APIKey: os.Getenv("ENVCTL_AI_API_KEY")}
			var err error
			s, err = client.Generate(ctx, fs.Arg(0))
			if err != nil {
				return err
			}
		}
		if err := spec.Validate(s); err != nil {
			return err
		}
		b, err := spec.Encode(s)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(b)
		closeErr := f.Close()
		if err = errors.Join(writeErr, closeErr); err != nil {
			return err
		}
		fmt.Fprintln(out, *output)
		return nil
	case "validate", "plan", "up":
		file := fs.String("file", "environment.yaml", "spec file")
		yes := fs.Bool("yes", false, "apply displayed plan (up only)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		s, err := spec.Decode(f)
		f.Close()
		if err != nil {
			return err
		}
		if command == "validate" {
			return printJSON(map[string]any{"valid": true, "spec": s})
		}
		p, err := application.Plan(ctx, s)
		if err != nil {
			return err
		}
		if err = showPlan(p); err != nil {
			return err
		}
		if command == "up" && *yes {
			return application.Apply(ctx, application.PlanPath(p))
		}
		return nil
	case "apply":
		if len(args) != 2 {
			return errors.New("apply requires a plan path")
		}
		return application.Apply(ctx, args[1])
	case "down":
		yes := fs.Bool("yes", false, "apply down plan")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("down requires environment name")
		}
		p, err := application.PlanDown(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		if err = showPlan(p); err != nil {
			return err
		}
		if *yes {
			return application.Apply(ctx, application.PlanPath(p))
		}
		return nil
	case "status":
		if len(args) != 2 {
			return errors.New("status requires environment name")
		}
		s, err := application.Status(ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(s)
	case "logs":
		if len(args) != 3 {
			return errors.New("logs requires environment and service names")
		}
		b, err := application.Logs(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err
	case "connections":
		reveal := fs.Bool("show-secrets", false, "explicitly reveal credentials to stdout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("connections requires environment name")
		}
		dir, unlock, err := st.Lock(fs.Arg(0))
		if err != nil {
			return err
		}
		defer unlock()
		var record app.Record
		if err = state.Read(filepath.Join(dir, "state.json"), &record); err != nil {
			return err
		}
		if record.Managed == nil {
			return errors.New("environment has not been applied")
		}
		results := []map[string]any{}
		for _, s := range record.Managed.Services {
			password := "[REDACTED]"
			if *reveal {
				b, e := state.ReadBytes(filepath.Join(dir, "secrets", s.Name+".password"), 128)
				if e != nil {
					return e
				}
				password = string(b)
			}
			port := 5432
			user := "app"
			database := "app"
			if s.Template == "redis" {
				port = 6379
				user = "default"
				database = "0"
			}
			entry := map[string]any{"service": s.Name, "template": s.Template, "container_host": s.Name, "container_port": port, "user": user, "database": database, "password": password}
			if s.HostPort > 0 {
				entry["host"] = "127.0.0.1"
				entry["port"] = s.HostPort
			}
			results = append(results, entry)
		}
		return printJSON(results)
	case "doctor":
		if len(args) != 1 {
			return errors.New("doctor takes no arguments")
		}
		target, err := application.Runtime.Check(ctx)
		if err != nil {
			return err
		}
		return printJSON(target)
	default:
		return fmt.Errorf("unknown command %q; run envctl help", strings.TrimSpace(command))
	}
}
