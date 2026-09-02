# Deploying freebuff-proxy with HTTPS

freebuff-proxy's dashboard and API can be deployed with automatic HTTPS (SSL/TLS) or accessed securely over encrypted tunnels.

---

## Option 1: Turnkey HTTPS with Caddy (Recommended for VPS with Domain)

The repository includes a built-in Caddy reverse proxy service in `docker-compose.yml` under the `https` profile. Caddy automatically provisions and renews free Let's Encrypt certificates.

### 1. Point DNS to your VPS
Create an `A` record pointing your domain (e.g. `proxy.yourdomain.com`) to your VPS public IP address. Ensure ports `80` and `443` are open on your VPS firewall.

### 2. Configure `.env`
Set `DOMAIN` and your admin password in `.env`:
```ini
DOMAIN=proxy.yourdomain.com
ACME_EMAIL=admin@yourdomain.com
ADMIN_TOKEN=your-strong-password
```

*(Optional)* To keep the backend port internal and only expose Caddy on 80/443, change `ports:` in `docker-compose.yml` from `"3457:3457"` to `"127.0.0.1:3457:3457"`.

### 3. Start with the `https` profile
```bash
docker compose --profile https up -d
```

Caddy will:
- Automatically issue an SSL certificate from Let's Encrypt
- Redirect HTTP (port 80) to HTTPS (port 443)
- Proxy traffic to `freebuff-proxy:3457` with `X-Forwarded-Proto: https`
- Automatically secure dashboard cookies (`Secure: true`)

Access your dashboard at `https://proxy.yourdomain.com/admin`.

---

## Option 2: Cloudflare Tunnel (No Open Ports or Dynamic IP)

If your VPS has no public IP, is behind CGNAT, or you prefer not to expose ports 80/443:

1. Install `cloudflared` on your VPS:
   ```bash
   curl -fsSL https://pkg.cloudflare.com/cloudflared-ascii.repo | sudo tee /etc/yum.repos.d/cloudflared.repo
   sudo apt install -y cloudflared # or your distro's package manager
   ```
2. Authenticate and create a tunnel:
   ```bash
   cloudflared tunnel create freebuff
   ```
3. Route DNS to your tunnel and run:
   ```bash
   cloudflared tunnel run --url http://127.0.0.1:3457 freebuff
   ```
Cloudflare terminates SSL at the edge and forwards HTTPS traffic to your local gateway.

---

## Option 3: Plain HTTP on Cloud VPS (No Domain / Quick Dev)

If you must access the dashboard directly via plain HTTP (`http://<vps-ip>:3457`) without a domain or TLS certificate:

1. In your `.env` file, set:
   ```ini
   ADMIN_INSECURE_HTTP=true
   ```
2. Rebuild/restart:
   ```bash
   docker compose up -d --build
   ```

> ⚠️ **Security Warning**: When `ADMIN_INSECURE_HTTP=true` is enabled, session cookies do not require HTTPS. On public networks, login credentials and session tokens travel in cleartext and may be intercepted. Use HTTPS in production.

---

## Option 4: Local SSH Port Forwarding

For a private remote instance without exposing the dashboard publicly:

```bash
ssh -L 3457:127.0.0.1:3457 user@your-vps-ip
```

Then open `http://127.0.0.1:3457/admin` in your local browser. Because `127.0.0.1` is treated as a secure origin by browsers, cookies remain fully functional without needing TLS.
