# FreeBuff Scripts & Tooling

## Quick Reference

| Action | Linux / macOS | Windows (PowerShell) | Windows (CMD / Explorer) |
| :--- | :--- | :--- | :--- |
| **Start Proxy** | `./start-proxy.sh` | `.\start-proxy.ps1` | `.\start-proxy.cmd` |
| **Generate Token** | `./gen-token.sh` | `.\gen-token.ps1` | `.\gen-token.cmd` |
| **Easy Install** | `./install.sh` | `.\install.ps1` | `.\install.cmd` |

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
1. Creates `.env` from `.env.example` if missing.
2. If `AUTH_TOKENS` is empty, offers to generate one now via browser login (appends to `.env`).
3. Starts the proxy and prints its address — point your AI client at `http://127.0.0.1:3457/v1`, model `deepseek/deepseek-v4-flash`.

> **Windows execution policy:** If PowerShell blocks `.ps1` execution, use the `.cmd` wrappers (`.\start-proxy.cmd`, `.\gen-token.cmd`, `.\install.cmd`) which run with `-ExecutionPolicy Bypass`.

---

## 2. Token Generator (`gen-token.*`)

`gen-token.sh` (Linux / macOS) and `gen-token.ps1` / `gen-token.cmd` (Windows) generate a FreeBuff auth token through a hardened headless OAuth flow:

1. Mints a fresh CLI-parity fingerprint (`enhanced-<43-char base64url>`, same shape as the official CLI's SHA-256 base64url fingerprint) and requests a login URL from the FreeBuff backend.
2. Auto-launches an isolated Private/Incognito browser window to prevent account linking.
3. Jitter-polls the authentication status every 5s (CLI parity).
4. Runs a zero-cost post-auth probe on `/api/v1/freebuff/session` (no instance header, no session slot consumed) to confirm account status/tier/risk and **refuses to save a banned account**.
5. Saves or appends the token to `.env`.

### Windows (PowerShell / CMD)

```powershell
# Interactive menu (recommended: Enter appends to .\.env)
.\gen-token.cmd

# Explicit modes:
.\gen-token.cmd -ToClipboard
.\gen-token.cmd -Save
.\gen-token.cmd -Append
.\gen-token.cmd -Incognito
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

## 4. Backward Compatibility Aliases

The following legacy script names are preserved as transparent forwarders:
- `gen-freebuff-token.ps1` / `gen-freebuff-token.sh` → `gen-token.*`
- `get-freebuff-token.ps1` / `get-freebuff-token.sh` → `gen-token.*`
- `install-freebuff-proxy.ps1` / `install-freebuff-proxy.sh` → `install.*`
- `install.bat` → `install.cmd`
