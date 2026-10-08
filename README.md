# vps-installer / envctl

A Linux Go CLI that turns a YAML specification or a natural-language description into a reviewed Docker Compose environment. The first catalog contains **PostgreSQL 17** and **Redis 7**. No daemon or AI connection is required to operate an existing environment.

This is an **MVP for single-user development and test hosts**, not a general-purpose VPS provisioner. Docker must already be installed. It does not install Linux packages, configure systemd/firewalls, deploy Kubernetes, remove persistent data, or migrate databases.

## Build and prerequisites

- Linux; Go 1.26 or newer to build.
- Docker Engine with a **local Unix socket** context and Compose supporting `up --wait-timeout` and `config --resolve-image-digests`.
- Registry access when preparing plans; image downloads during apply.
- Approximately 1 GiB of service memory limits for the default two-service example, plus OS/Docker overhead.

```bash
git clone https://github.com/haarardin/vps-installer.git
cd vps-installer
go build -trimpath -o bin/envctl ./cmd/envctl
./bin/envctl doctor
```

Access to the Docker daemon is a privileged capability. Run only on a host and Docker context you intend to manage. Podman compatibility is not currently implemented or claimed.

## First environment, without an AI provider

```bash
./bin/envctl init --name backend-dev --publish --output environment.yaml
./bin/envctl validate --file environment.yaml
./bin/envctl plan --file environment.yaml
```

`plan` prints JSON containing `plan`, `changes`, pinned `images`, and an absolute `path`. Review it, then explicitly apply that file:

```bash
./bin/envctl apply /absolute/path/printed/by/plan/plan.json
./bin/envctl status backend-dev
./bin/envctl logs backend-dev database
./bin/envctl connections backend-dev
```

For a combined plan-and-apply operation:

```bash
./bin/envctl up --file environment.yaml --yes
```

Without `--yes`, `up` only prepares a plan. `apply PATH` is itself explicit authorization to execute that saved plan. State or Docker resource changes can invalidate a plan; generate a new one instead of bypassing the check.

`init` publishes no ports unless `--publish` is supplied. Published services are always bound to **127.0.0.1** on the Docker host. On a VPS this means the VPS loopback address, not your laptop. Use your own SSH tunnel if needed. Inside the project network, use service names and container ports shown by `connections`.

## Environment format

```yaml
api_version: envctl/v1alpha1
name: backend-dev
services:
  - name: database
    template: postgres
    version: "17"
    host_port: 5432
    persistent: true
  - name: cache
    template: redis
    version: "7"
    host_port: 6379
    persistent: false
```

`host_port: 0` means unpublished. PostgreSQL requires persistence. Redis may use either a named volume with AOF or an ephemeral tmpfs. Ports are TCP/IPv4. Unknown keys, duplicate keys, aliases/anchors, unsupported templates, invalid names, and duplicate published ports are rejected. There is no arbitrary shell, image, host mount, or privileged-mode field.

You may add services or change a service's published port. Removing/renaming services, changing templates/versions/storage, and rotating credentials are deliberately rejected in this MVP. Plan a separate migration for those changes.

## Optional natural-language input

Configure an **OpenAI-compatible chat-completions endpoint** supporting JSON object output. The endpoint is the complete URL, not a base URL. The model is explicit; there is no hardcoded paid-provider default.

```bash
export ENVCTL_AI_ENDPOINT='http://127.0.0.1:8080/v1/chat/completions'
export ENVCTL_AI_MODEL='your-compatible-model'
# For a provider that requires authentication, set ENVCTL_AI_API_KEY securely.

./bin/envctl create --output environment.yaml \
  'Create backend-dev with persistent PostgreSQL and ephemeral Redis. Publish both on localhost for my Go backend.'
./bin/envctl validate --file environment.yaml
./bin/envctl plan --file environment.yaml
```

HTTPS is required except for loopback HTTP. Redirects are disabled. `create` writes a candidate specification; it never calls Docker. The request contains only the description and fixed catalog instructions. Do not put credentials into descriptions. Unsupported/ambiguous requests fail rather than producing arbitrary commands. AI output is untrusted and passes through the same strict validator; review the generated spec and plan because the model can still misunderstand intent.

AI service availability and billing depend on your provider. No live AI call is needed in CI. YAML operation remains fully available offline from AI (Docker planning still needs registry access).

## Credentials and data

```bash
# Explicitly show credentials to this terminal; do not pipe into shared logs.
./bin/envctl connections --show-secrets backend-dev

# Review a stop/removal plan (keeps named volumes and credentials):
./bin/envctl down backend-dev
# Apply immediately after planning:
./bin/envctl down --yes backend-dev
# Recreate containers with retained PostgreSQL data:
./bin/envctl up --file environment.yaml --yes
```

Passwords are random and reused. PostgreSQL receives a password file; Redis uses an ACL with a password hash, plus a separate passwordless health user allowed only `PING`. Application Redis access still requires the default-user password. Credentials are redacted from `connections` unless explicitly requested. `logs` masks the known raw passwords; it cannot guarantee detection of arbitrary encodings or other application secrets. Logs never go to the AI provider automatically.

State lives in `$XDG_STATE_HOME/envctl` or `~/.local/state/envctl`; override with the global `--state-dir` option **before the command**. Keep this directory: deleting it loses the inventory, pinned images and credentials needed to manage retained volumes.

```bash
./bin/envctl --state-dir /private/envctl --context default status backend-dev
```

Directories are mode 0700. JSON state and normal secret files are 0600. The PostgreSQL mounted password and Redis ACL files are 0444 **inside the private 0700 directory** so image-specific service UIDs can read the bind-mounted files. Standalone Compose secrets are filesystem mounts, not encrypted vault storage. Root/Docker access can read them. Back up both state and database data separately; this tool does not implement backup/restore.

## Reliability model

- Each environment has a random ID, unique Compose project name and ownership labels.
- Changes are planned against the state revision, local daemon identity, credential fingerprints and observed resources.
- Images resolve to digests; subsequent plans reuse existing image locks.
- Apply regenerates Compose from validated typed inputs; it does not execute an edited generated manifest.
- Exclusive OS file locks prevent concurrent mutations using the same state root.
- State writes use temporary files, fsync and rename; a journal record precedes runtime mutation.
- A failed start leaves resources and data for diagnosis. No automatic `down`, volume deletion or database rollback.
- `status` includes the last operation phase and fresh Docker resources. A previous `ready` phase is historical; inspect current resource health too.
- After interruption, inspect `status`/`logs` and create a fresh `plan`. A new apply reconciles supported services. Missing existing credentials fail closed and must be restored.

Compose is not transactional. Containers may exist after cancellation or failure. Changes can restart containers and cause downtime. The state root is trusted and single-user; this is not a distributed lock or multi-tenant security boundary. Snapshot checks cannot prevent a different Docker client changing resources immediately after the check.

## Tests and CI

```bash
go vet ./...
go test -race -count=1 ./...
go test ./internal/spec -run='^$' -fuzz=FuzzDecode -fuzztime=10s
# Run ONLY on an isolated Docker test host:
ENVCTL_INTEGRATION=1 go test -tags=integration -count=1 -timeout=9m -v ./integration
```

Ordinary tests use fake runtime/HTTP adapters, temporary directories and short helper processes; they do not need Docker, external AI, VPS credentials, or registry access. The integration test runs real PostgreSQL/Redis, verifies SQL and authenticated Redis access, reapplies without replacing containers, and checks data/credentials across down/up. Cleanup is limited to its randomly named test project and its test volume.

GitHub Actions runs unit/race/vet/fuzz/build before integration, and publishes the Linux binary and coverage file as workflow artifacts. These checks do not certify production readiness: reboot, disk exhaustion, backups, real operational load and recovery on your VPS still need acceptance testing.

## Code map and project status

- `internal/spec`: strict YAML/JSON spec and safe change rules.
- `internal/compose`: typed renderer, isolated process runner, digest resolution, ownership checks.
- `internal/state`: private state, file locks, atomic writes, stable credentials.
- `internal/app`: plan/apply/status/down orchestration and operation journal.
- `internal/intent`: optional AI-to-spec adapter.
- `internal/cli`: command dispatch and machine-readable output.
- `integration`: opt-in Docker lifecycle test.

See [implementation notes](docs/MVP.md) and [original architecture proposal](docs/ARCHITECTURE-PROPOSAL.md). The proposal is a roadmap; the implementation notes identify the actual MVP scope. Remote SSH execution, broader service catalogs, explicit image upgrades, data purge, service removal, and backup/restore are later work.
