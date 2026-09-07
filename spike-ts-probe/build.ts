// THROWAWAY SPIKE build script. Mirrors the upstream pattern from
// reference/freebuff/cli/scripts/build-binary.ts in simplified form:
//   bun build <entry> --compile --production --target=<bun-target>
//   --outfile=<bin/> --sourcemap=none --define ...
// Not for merge to main.
import { spawnSync } from "node:child_process";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { mkdirSync, existsSync } from "node:fs";

const __dirname = dirname(fileURLToPath(import.meta.url));

const mappings: Record<string, string> = {
  "linux-x64": "bun-linux-x64",
  "linux-arm64": "bun-linux-arm64",
  "darwin-x64": "bun-darwin-x64",
  "darwin-arm64": "bun-darwin-arm64",
  "win32-x64": "bun-windows-x64",
};

const key = `${process.platform}-${process.arch}`;
const bunTarget = process.env.OVERRIDE_TARGET ?? mappings[key];
if (!bunTarget) throw new Error(`Unsupported build target: ${key}`);

const version = process.argv[2] ?? "0.0.0-spike";
const exe = process.platform === "win32" ? "spike-ts-probe.exe" : "spike-ts-probe";
const binDir = join(__dirname, "bin");
if (!existsSync(binDir)) mkdirSync(binDir, { recursive: true });
const outfile = join(binDir, exe);

console.log(`Building spike-ts-probe @ ${version} target=${bunTarget}`);
const args = [
  "build",
  join(__dirname, "server.ts"),
  "--compile",
  "--production",
  "--no-compile-autoload-bunfig",
  `--target=${bunTarget}`,
  `--outfile=${outfile}`,
  "--sourcemap=none",
  "--define",
  'process.env.NODE_ENV="production"',
];
const res = spawnSync("bun", args, { stdio: "inherit" });
if (res.status !== 0) throw new Error(`bun build failed status=${res.status}`);
console.log(`Built ${outfile}`);
