// THROWAWAY SPIKE: minimal Bun probe server. Not for merge to main.
// Endpoints:
//   GET /admin/status        admin probe (JSON)
//   GET /v1/synthetic-sse    synthetic SSE relay, no upstream
//   GET /                    one static file (bundle-embedded, since a
//                            --compile binary cannot read sibling files
//                            via import.meta URL; same lesson as upstream
//                            tree-sitter.wasm sibling handling)
// Env: PORT (default 18731).
import INDEX from "./public/index.html" with { type: "text" };

const PORT = Number(process.env.PORT ?? "18731");
const VERSION = "0.0.0-spike";
const startedAt = Date.now();

function sseStream(chunks: number): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  let i = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (i < chunks) {
        i++;
        const payload = JSON.stringify({ i, text: "chunk-" + i });
        controller.enqueue(enc.encode(`data: ${payload}\n\n`));
      } else {
        controller.enqueue(enc.encode("data: [DONE]\n\n"));
        controller.close();
      }
    },
  });
}

const server = Bun.serve({
  port: PORT,
  async fetch(req) {
    const url = new URL(req.url);
    if (url.pathname === "/admin/status" && req.method === "GET") {
      return Response.json({
        ok: true,
        version: VERSION,
        uptime_s: Math.floor((Date.now() - startedAt) / 1000),
      });
    }
    if (url.pathname === "/v1/synthetic-sse" && req.method === "GET") {
      const raw = Number(url.searchParams.get("chunks") ?? "20");
      const chunks = Number.isFinite(raw) ? Math.min(Math.max(Math.floor(raw), 1), 500) : 20;
      return new Response(sseStream(chunks), {
        headers: {
          "Content-Type": "text/event-stream",
          "Cache-Control": "no-cache",
          Connection: "keep-alive",
        },
      });
    }
    if ((url.pathname === "/" || url.pathname === "/index.html") && req.method === "GET") {
      return new Response(INDEX, { headers: { "Content-Type": "text/html; charset=utf-8" } });
    }
    return new Response("not found", { status: 404 });
  },
});

console.log(`spike-ts-probe listening port=${server.port} version=${VERSION}`);
