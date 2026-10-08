# MVP implementation decisions

The initial architecture proposal describes a larger target. This version implements a narrow end-to-end vertical slice with real Docker and an optional AI adapter.

| Area | Implemented | Deferred |
|---|---|---|
| Input | Strict versioned YAML/JSON, max size/depth, no aliases/duplicate keys | Interactive ambiguity resolution, editing by prompt |
| Catalog | Embedded typed PostgreSQL 17 / Redis 7 templates | Pluggable catalog, arbitrary images/builds |
| Planning | Create/add/port update/reconcile/down, saved fingerprint, observed snapshot, target and revision binding | Service removal, migration/upgrade plans |
| Images | Compose registry resolution to digest, reused locks | Explicit refresh/upgrade workflow, independent catalog version distribution |
| Runtime | Local Unix Docker context, Compose health wait, bounded logs, ownership checks | Podman, remote context, SSH executor, installation of Docker |
| State | Atomic snapshot, per-operation durable record, flock | Distributed locking, multi-user access control, continuously running controller |
| Recovery | Fresh plan and apply after partial failure; retained resources | Automated rollback or missing-secret recovery, dedicated reconcile command |
| Data | Persistent PostgreSQL, optional persistent Redis, no data deletion commands | Backup/restore, purge and database migrations |
| AI | Configurable compatible chat-completions adapter, JSON validation, timeout, no redirects | Provider-specific SDK, streaming, automatic retries, tool execution |
| UI | CLI, JSON plans/status/connections | TUI/web UI |

## Operation ordering

1. Plan validates spec and changes, checks local target and ownership, renders/resolves/pins/validates Compose, writes the saved plan.
2. Apply validates the plan, locks the state, checks base revision/target/observation, regenerates the artifact, writes an applying operation record and state revision.
3. Ensure stable credentials, validate generated config, run Compose and inspect health.
4. Record the outcome; only healthy `up` updates last-successful spec. A failure after runtime success but before durable state completion is reported as a persistence failure requiring inspection.

A process killed mid-operation may leave `applying` in the journal. It never implies that Docker did nothing. Generate a fresh plan after inspection; do not replay an old plan. The operation journal intentionally stores phase metadata rather than raw command output or model prompts.

## Test boundaries

Pure tests cover decoding, validation, change policy, rendering and adapter decisions. Component tests cover real temporary files, lock contention, subprocess cancellation and mock HTTP. Application tests use a fake runtime and real temporary durable state to test ordering and recovery behavior. Docker tests are separate, opt-in and gated after ordinary tests in CI.

The first implementation does not enforce a blanket coverage percentage. Important failure-path tests include stale plan/target, unhealthy service, failed startup, symlink rejection, credentials reuse, edited manifest, write failure after runtime success and refusal of foreign resource ownership. Untested platform/failure scenarios remain acceptance work, not inferred guarantees.

## Host assumptions

Linux only. One trusted user and state root per environment. No adversarial same-UID process modifying state concurrently; filesystem and Docker permissions are part of the trust boundary. Symlink checks reject ordinary unsafe paths but are not a sandbox against a hostile process with the same host privileges. Docker resource labels enforce accidental ownership separation, not authentication against Docker administrators.

Port availability is ultimately checked by Docker at bind time. A port check before starting could race and would not replace that result. Runtime errors preserve Docker diagnostics; they are not heuristically classified into precise error codes unless supported by evidence.

## Primary references

- https://docs.docker.com/reference/cli/docker/compose/config/
- https://docs.docker.com/reference/cli/docker/compose/up/
- https://docs.docker.com/reference/cli/docker/compose/down/
- https://docs.docker.com/compose/how-tos/use-secrets/
- https://docs.docker.com/engine/security/
