// THROWAWAY SPIKE bench harness. Hermetic local only. Not for merge.
import { spawn } from "node:child_process";

const BIN = process.argv[2] ?? "";
const PORT = process.argv[3] ?? "18731";
const N = Number(process.argv[4] ?? "200");
if (!BIN) throw new Error("usage: bench.ts <binary> <port> [n]");

async function rssBytes(pid: number): Promise<number> {
  if (process.platform === "win32") {
    const p = Bun.spawnSync(["powershell", "-NoProfile", "-Command",
      `(Get-Process -Id ${pid} -ErrorAction Stop).WorkingSet64`]);
    const t = p.stdout.toString().trim();
    const v = Number(t);
    return Number.isFinite(v) ? v : -1;
  }
  const p = Bun.spawnSync(["ps", "-o", "rss=", "-p", String(pid)]);
  const kb = Number(p.stdout.toString().trim());
  return Number.isFinite(kb) ? kb * 1024 : -1;
}

const t0 = Date.now();
const child = spawn(BIN, [], {
  env: { ...process.env, PORT },
  stdio: ["ignore", "pipe", "pipe"],
});
let readyLine = "";
child.stderr.on("data", (d: Buffer) => { readyLine += d.toString(); });
child.stdout.on("data", (d: Buffer) => { readyLine += d.toString(); });

let startupMs = -1;
const base = `http://127.0.0.1:${PORT}`;
for (let i = 0; i < 200; i++) {
  await Bun.sleep(50);
  try {
    const r = await fetch(`${base}/admin/status`);
    if (r.ok) { await r.text(); startupMs = Date.now() - t0; break; }
  } catch { /* not up yet */ }
}
if (startupMs < 0) { child.kill(); throw new Error("server never became ready; output:\n" + readyLine); }
await Bun.sleep(1000); // settle to idle
const rss = await rssBytes(child.pid!);

// correctness spot checks
const admin = await (await fetch(`${base}/admin/status`)).json() as any;
const idx = await (await fetch(`${base}/`)).text();
const staticOk = idx.includes("spike-ts-probe static ok");

// throughput: sequential synthetic SSE GETs, chunks=20, full body consume
const t1 = Date.now();
let bytes = 0;
for (let i = 0; i < N; i++) {
  const r = await fetch(`${base}/v1/synthetic-sse?chunks=20`);
  const buf = await r.arrayBuffer();
  bytes += buf.byteLength;
}
const wallMs = Date.now() - t1;
child.kill();

console.log(JSON.stringify({
  startup_ms: startupMs,
  rss_idle_bytes: rss,
  rss_idle_mib: rss > 0 ? +(rss / 1048576).toFixed(2) : -1,
  sse_requests: N,
  sse_wall_ms: wallMs,
  sse_req_per_s: +(N / (wallMs / 1000)).toFixed(1),
  sse_bytes: bytes,
  admin_ok: admin?.ok === true,
  static_ok: staticOk,
}));
