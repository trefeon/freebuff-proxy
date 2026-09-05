# Package Contract: `backend/internal/cli`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Process modes and serve orchestration. `main.go` is a thin flag parser;
`cli.Serve(configPath, verbose, version)` runs the default serve mode:
load config → build logger → registry + refresh loop → one upstream client +
session manager per token → pool (`Start`) → HTTP server → graceful drain on
shutdown signals. Per-mode logic lives in subpackages
(`port`, `setup`, `update`, `service`, `doctor`, `refreshtoken`, `validate`).

## Allowed dependencies

Bounded subset (archtest matrix): `cli/port`, `clicreds`, `config`, `egress`
(doctor probes), `logring`, `notify`, `pool`, `registry`, `server`,
`session`, `telemetry`, `updatecheck`, `upstream`. Each subpackage has its
own narrower set. `cli`
is the only package that talks to the OS surface (signals, console hold,
exe-adjacent `.env` warning, cleartext-TLS warning).

## Forbidden dependencies

`convert`, `dashboard`, `runs` (reached via pool), anything that would make
entrypoints smart. Mode logic must not duplicate `server` behavior; modes
are diagnostics/setup utilities, not second gateways.

## Critical invariants

- `Serve` returns an exit code (0 normal, 1 server failure); the caller maps
  it to `os.Exit`. Do not `os.Exit` inside subpackages except main-mapped
  mode runners.
- Shutdown: `os.Interrupt` + `SIGTERM`; on Windows Ctrl+C and Ctrl+Break both
  arrive as SIGINT (pinned by `TestCtrlBreakDrainsGracefully`). The drain is
  graceful and bounded; every background worker joins the shutdown context.
- Log-level precedence: `LOG_LEVEL` config wins, `-v` → debug, dev builds
  (unstamped version) default debug.
- Service units (Task Scheduler XML, systemd unit, launchd plist) are
  generated in code parity with the checked-in `scripts/` templates; the
  Windows service/unit tests pin that parity.
- Self-update (`update/`): SHA-256-verified against `checksums.txt`, atomic
  swap; never overwrite a running binary on Windows without the deferred-swap path.
- Exe-adjacent `.env` warning and admin-cleartext warning fire at the right
  moments; they are operator-facing diagnostics, not logs to silence.

## Tests that protect it

`cli_test.go` (flags, Serve wiring), `cli_windows_test.go`
(ctrl+break, service units), `doctor/`, `update/`, `setup/`, `validate/`,
`port/`, `service/`, `refreshtoken/` tests, cmd E2E suite
(serve/drain/port-conflict/config-json/bridge/doctor/test-token/setup/update).

## Safe modification patterns

- New CLI flag: wire in `main.go` + mode exclusivity warning
  (`ModeFlagsExclusiveWarning`) + `cli/*` implementation + README CLI table
  + E2E coverage where it touches the serve path.
- New background worker in Serve: bind to the shutdown context in the same
  commit; extend the lifecycle/shutdown tests.