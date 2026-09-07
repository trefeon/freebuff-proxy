# THROWAWAY SPIKE results: Bun+TS as backend alternative

Branch: spike/ts-binary-probe. All code under spike-ts-probe/ is throwaway.
No merge to main is proposed. No credentials and no upstream traffic were
used. Every measurement below is hermetic local loopback on one machine.

Machine: Windows 11 Pro x64, i7-11800H, Bun 1.3.14, Go 1.26.6.
Method: `bun build server.ts --compile --production
--target=bun-windows-x64 --outfile=bin/ --sourcemap=none`, mirroring
reference/freebuff/cli/scripts/build-binary.ts (target map, --compile,
--production, --no-compile-autoload-bunfig, --sourcemap=none, --define).
Go baseline is a minimal net/http server in gobaseline/ serving the same
three routes (admin JSON, 20-chunk synthetic SSE, one static file).
Bench harness is bench.ts: spawn, poll /admin/status every 50 ms until
ready, settle 1 s, read WorkingSet64, then 200 sequential SSE GETs
(chunks=20) with full body consume.

## Numbers

| Metric | Bun compiled binary | Go baseline binary |
|---|---|---|
| Binary size (bytes) | 98,480,216 (93.9 MiB) | 8,449,024 (8.1 MiB) |
| Startup to listen, warm | 136 ms (repeat: 141 ms) | 487 ms |
| Startup to listen, cold first launch | 1173 ms (Windows AV scan on fresh binary) | 954 ms |
| RSS idle (WorkingSet64) | 49.2 MiB | 11.4 MiB |
| Synthetic SSE, sequential, 200x20 chunks | 3390 req/s (repeat: 3571, 3922) | 2083 req/s (repeat: 2222) |

Go claim comparison: the repo has no published SSE req/s claim. The
standing Go-side statement is structural, not numeric: convert uses a
sync.Pool zero-allocation fast path for SSE chunk sanitize
(backend/internal/convert/sse.go, accumulator_xml.go). This probe measured
plain synthetic relay throughput instead, so the table above is the first
apples-to-apples number for both runtimes on identical routes. Bun served
the synthetic shape about 1.6x faster sequentially on this box, while
using about 4.3x the idle RSS and producing an 11.7x larger binary.

## TLS fingerprint (utls replacement survey, no implementation)

What must be replaced: backend/internal/stealth builds JA3 browser
impersonation on github.com/refraction-networking/utls ClientHello presets
(selected via the hidden TLS_FINGERPRINT key; empty default stays
CLI-faithful plain baseline).

Options surveyed in Bun:

1. Bun.fetch / Bun.serve TLS options. Cover cert, key, CA and limited
   cipher selection only. No ClientHello extension ordering, no GREASE
   control, no preset impersonation.
2. node:tls through Bun. Same ceiling as Node: cipher list control only,
   no raw ClientHello crafting.
3. Native impersonation libs via child process or sidecar
   (curl-impersonate family pattern). Works but adds a second binary and
   IPC to every upstream dial, which cancels the single-binary motive.
4. bun:ffi against BoringSSL or a port of utls ClientHello logic. Full
   reimplementation of what utls already does, plus per-target native
   build burden. Out of proportion for a probe.
5. Keep upstream TLS in Go (utls stays) and put Bun anywhere else. Viable
   but concedes the exact layer the proxy exists to control.

Finding: Bun (BoringSSL) exposes no ClientHello or JA3/JA4 control in
fetch or tls as of Bun 1.3.14; a community request for JA3 passthrough
(oven-sh/bun issue 11368) is still open. Sources: bun.com/docs/runtime/http/tls
and the linked issue thread. There is no drop-in utls replacement inside
the Bun runtime.

## Recommendation

Stop: keep Go for the backend; Bun fails the two load-bearing needs
(single small binary and utls-grade upstream TLS control) while its one
win (synthetic SSE speed) does not matter against real upstream latency.
