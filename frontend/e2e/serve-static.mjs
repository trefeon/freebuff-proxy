import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { existsSync, statSync } from 'node:fs';
import { join, extname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = fileURLToPath(new URL('.', import.meta.url));
// dist is at ../internal/dashboard/dist relative to frontend/e2e
const dist = resolve(__dirname, '../../internal/dashboard/dist');
const port = 4173;
const host = '127.0.0.1';

const mime = {
  '.html': 'text/html',
  '.js': 'application/javascript',
  '.css': 'text/css',
  '.json': 'application/json',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
};

const server = createServer(async (req, res) => {
  try {
    const url = new URL(req.url, `http://${host}:${port}`);
    let pathname = url.pathname;
    // SPA fallback: /admin/* serves index.html
    if (pathname === '/admin' || pathname === '/admin/' || pathname.startsWith('/admin/')) {
      // If it's an asset under /admin/assets/... serve that file
      if (pathname.startsWith('/admin/assets/')) {
        const filePath = join(dist, pathname.replace('/admin/', ''));
        if (existsSync(filePath) && statSync(filePath).isFile()) {
          const ext = extname(filePath);
          res.writeHead(200, { 'Content-Type': mime[ext] || 'application/octet-stream' });
          res.end(await readFile(filePath));
          return;
        }
      }
      // Otherwise serve index.html
      const indexPath = join(dist, 'index.html');
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end(await readFile(indexPath));
      return;
    }
    // Also handle root redirect
    if (pathname === '/' || pathname === '') {
      res.writeHead(302, { Location: '/admin/' });
      res.end();
      return;
    }
    res.writeHead(404);
    res.end('Not found');
  } catch (e) {
    res.writeHead(500);
    res.end(String(e));
  }
});

server.listen(port, host, () => {
  console.log(`[serve-static] serving ${dist} at http://${host}:${port}/admin/`);
});
