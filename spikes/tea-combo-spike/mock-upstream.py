#!/usr/bin/env python3
"""Mock OpenAI-compatible upstream for the combo spike.

bad-model  -> always 503 (retryable)
bad400     -> always 400 (non-retryable)
good-model -> 200 with a recognisable body
Every request is appended to <spike-dir>/mock.log so we can prove call order.
"""
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = "<spike-dir>/mock.log"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _send(self, status, body):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path.endswith("/models"):
            self._send(200, {"object": "list", "data": [
                {"id": "bad-model", "object": "model"},
                {"id": "bad400", "object": "model"},
                {"id": "good-model", "object": "model"},
            ]})
        else:
            self._send(404, {"error": "nope"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        payload = json.loads(self.rfile.read(length) or b"{}")
        model = payload.get("model", "")
        with open(LOG, "a") as fh:
            fh.write(f"{model}\n")
        if model == "bad-model":
            self._send(503, {"error": {"message": "mock overloaded", "type": "server_error"}})
        elif model == "bad400":
            self._send(400, {"error": {"message": "mock bad request", "type": "invalid_request_error"}})
        else:
            self._send(200, {
                "id": "chatcmpl-mock", "object": "chat.completion", "created": 1, "model": model,
                "choices": [{"index": 0, "finish_reason": "stop",
                             "message": {"role": "assistant", "content": f"served-by:{model}"}}],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            })


HTTPServer(("127.0.0.1", 18399), Handler).serve_forever()
