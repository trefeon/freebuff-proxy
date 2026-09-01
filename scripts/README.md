# FreeBuff Scripts & Tooling

## Quick Reference

| Action | Linux / macOS | Windows (PowerShell) | Windows (CMD / Explorer) |
| :--- | :--- | :--- | :--- |
| **Start Proxy** | `./start-proxy.sh` | `.\start-proxy.ps1` | `.\start-proxy.cmd` |
| **Generate Token** | `./gen-freebuff-token.sh` | `.\gen-freebuff-token.ps1` | (use `.\gen-freebuff-token.ps1` in PowerShell) |
| **Easy Install** | `./install.sh` | `.\install.ps1` | `.\install.cmd` |
| **Sync Upstream** | `./sync-upstream.sh` | `.\sync-upstream.ps1` | `.\sync-upstream.cmd` |
---

## 1. Run the proxy — one command

The extracted release folder contains `freebuff-proxy` (Linux/macOS) or `freebuff-proxy.exe` (Windows) plus these scripts.

**Windows:** unzip, right-click the extracted folder → **Open in Terminal**, then:

```powershell
.\start-proxy.cmd
```

**Linux / macOS:** unzip, right-click the extracted folder → **Open in Terminal**, then:

```bash
./start-proxy.sh
```

`start-proxy.*` handles the entire setup automatically:
1. Creates `.env` in your **platform config directory** from `.env.example` if missing (see the table below).
2. If `AUTH_TOKENS` is empty, offers to generate one now via browser login (appends to `.env`).
3. Starts the proxy and prints its address — point your AI client at `http://127.0.0.1:3457/v1`, model `deepseek/deepseek-v4-flash` (`.env` is resolved by the runtime, so it works from any directory).

### Installer flags

`install.sh` / `install-freebuff-proxy.sh` (bash) and `install.ps1` / `install-freebuff-proxy.ps1` (PowerShell) accept the following overrides:

| Behavior | bash | PowerShell |
| :--- | :--- | :--- |
| Install root (binary + template) | `--prefix <dir>` / `--dir <dir>` | `-Dir <dir>` |
| Target `.env` file | `--env-file <path>` | `-EnvFile <path>` |
| Skip token prompt | `--skip-token` | `-SkipToken` |
| Do not create `.env` | `--no-env` | `-NoEnv` |
| Force re-download | `--force` | `-Force` |
| Skip the interactive menu | `--method=binary` / `--method=docker` / `--method=bridge` | *(menu only)* |

The default install root is the platform directory (see [README → Where the files are installed](../README.md#where-the-files-are-installed)); `--dir <dir>` / `-Dir <dir>` and a dev-clone checkout preserve the legacy "config in the current directory" behavior.

> **Windows execution policy:** If PowerShell blocks `.ps1` execution, use the `.cmd` wrappers (`.\start-proxy.cmd`, `.\gen-token.cmd`, `.\install.cmd`) which run with `-ExecutionPolicy Bypass`.

---

## 2. Token Generator (`gen-token.*`)

`gen-token.sh` (Linux / macOS) and `gen-token.ps1` / `gen-token.cmd` (Windows) generate a FreeBuff auth token through a hardened headless OAuth flow:

1. Mints a fresh CLI-parity fingerprint (`enhanced-<43-char base64url>`, same shape as the official CLI's SHA-256 base64url fingerprint) and requests a login URL from the FreeBuff backend.
2. Auto-launches an isolated Private/Incognito browser window to prevent account linking.
3. Jitter-polls the authentication status every 5s (CLI parity).
4. Saves or appends the token to `.env`.

> **Privacy default:** the fresh token is **never sent back to the server**
> after login. The old auto-probe (ban/tier check on
> `/api/v1/freebuff/session`) is now opt-in: pass `-Verify`
> (PowerShell) or `--verify` (bash). With `--verify`, the probe sends no
> instance header (no session slot consumed) and refuses to save a banned
> account.

### Windows (PowerShell / CMD)

```powershell
# Interactive menu (recommended: Enter appends to .\.env)
.\gen-token.cmd

# Explicit modes:
.\gen-token.cmd -ToClipboard
.\gen-token.cmd -Save
.\gen-token.cmd -Append
.\gen-token.cmd -Incognito
.\gen-token.cmd -Verify   # opt-in post-auth probe
```

### Linux / macOS (Bash)

```bash
# Interactive menu (recommended: Enter appends to ./.env)
./gen-token.sh

# Explicit modes:
./gen-token.sh --clipboard
./gen-token.sh --save
./gen-token.sh --append
./gen-token.sh --incognito
```

---

## 3. Options Table

| Behavior | Windows | Linux / macOS |
| :--- | :--- | :--- |
| Interactive menu (default) | *(no flags)* | *(no flags)* |
| Append to `.env` AUTH_TOKENS | `-Append` | `--append` |
| Target specific `.env` file | `-EnvFile <path>` | `--env <path>` |
| Copy to clipboard | `-ToClipboard` | `--clipboard` |
| Save to CLI credentials file | `-Save` | `--save` |
| Manual Incognito (print URL) | `-Incognito` | `--incognito` |

---

## 4. Upstream Synchronization (`sync-upstream.*` / `check-upstream.sh`)

Automates fetching upstream changes from `CodebuffAI/freebuff`, syncing the five pinned model registry definitions (`backend/internal/registry/testdata/upstream/`), checking hash parity, and running tests.

### Windows (PowerShell / CMD)

```powershell
# Full sync: fetch upstream, copy changes, verify hashes, run registry tests
.\sync-upstream.cmd

# Check drift without writing files
.\sync-upstream.cmd -CheckOnly

# Sync and run the full test suite
.\sync-upstream.cmd -TestAll
```

### Linux / macOS / Git Bash

```bash
# Full sync
./sync-upstream.sh

# Check drift only
./sync-upstream.sh --check

# Sync and run the full test suite
./sync-upstream.sh --test-all
```

---

## 5. Backward Compatibility Aliases

The following legacy script names are preserved as transparent forwarders:
- `gen-token.ps1` / `gen-token.sh` → `gen-freebuff-token.*` (canonical generator)
- `get-freebuff-token.ps1` / `get-freebuff-token.sh` → `gen-freebuff-token.*`
- `install-freebuff-proxy.ps1` / `install-freebuff-proxy.sh` → `install.*`
- `install.bat` → `install.cmd`
