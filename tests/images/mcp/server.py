"""A minimal MCP server (streamable HTTP, JSON-RPC over POST /mcp).

TOKEN: every request must carry `Authorization: Bearer <TOKEN>`, else 401.
SSE=1: answer requests as a text/event-stream instead of JSON.
GET /calls lists what was asked (method, tool, whether the auth was right),
never the auth value itself, so tests can see what reached the server.
"""

import json
import os
import threading
import uuid
import http.server

TOKEN = os.environ["TOKEN"]
SSE = os.environ.get("SSE") == "1"
calls = []
lock = threading.Lock()


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _send(self, status, body=b"", ctype="application/json", headers=()):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in headers:
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/calls":
            with lock:
                body = json.dumps(calls).encode()
            return self._send(200, body)
        self._send(405 if self.path == "/mcp" else 404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        try:
            msg = json.loads(self.rfile.read(length) or b"{}")
        except ValueError:
            return self._send(400)
        auth = self.headers.get("Authorization") == f"Bearer {TOKEN}"
        method = msg.get("method", "")
        params = msg.get("params") or {}
        with lock:
            calls.append({"method": method, "tool": params.get("name") if method == "tools/call" else None,
                          "auth": auth, "session": self.headers.get("Mcp-Session-Id")})
        if self.path != "/mcp":
            return self._send(404)
        if not auth:
            return self._send(401, b'{"error":"unauthorized"}', headers=[("WWW-Authenticate", "Bearer")])
        if "id" not in msg:
            return self._send(202)  # a notification
        headers = []
        if method == "initialize":
            result = {"protocolVersion": params.get("protocolVersion") or "2025-06-18",
                      "capabilities": {"tools": {}}, "serverInfo": {"name": "lux-test-mcp", "version": "1"}}
            headers.append(("Mcp-Session-Id", uuid.uuid4().hex))
        elif method == "tools/list":
            result = {"tools": [{"name": "echo", "description": "Echoes its text back.",
                                 "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}},
                                                 "required": ["text"]}}]}
        elif method == "tools/call" and params.get("name") == "echo":
            text = (params.get("arguments") or {}).get("text", "")
            result = {"content": [{"type": "text", "text": f"echo: {text}"}], "isError": False}
        elif method == "tools/call":
            result = {"content": [{"type": "text", "text": f"unknown tool {params.get('name')}"}], "isError": True}
        elif method == "ping":
            result = {}
        else:
            reply = {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": f"unknown method {method}"}}
            return self._send(200, json.dumps(reply).encode(), headers=headers)
        reply = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result})
        if SSE:
            return self._send(200, f"event: message\ndata: {reply}\n\n".encode(), "text/event-stream", headers)
        self._send(200, reply.encode(), headers=headers)

    def log_message(self, *a):
        pass


http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
