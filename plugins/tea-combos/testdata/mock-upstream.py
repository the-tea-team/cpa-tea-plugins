#!/usr/bin/env python3
"""Deterministic OpenAI-compatible upstream for the tea-combos proofs.

Usage: mock-upstream.py <request-log> [port]

Models:
  rr-a, rr-b, rr-c -> 200 JSON chat completion (round-robin distribution proof)
  bad400           -> 400 non-retryable client error (chain stop proof)
  bad503           -> 503 retryable upstream error (fallback proof)
  stream-good      -> SSE, three data chunks with a short delay, then [DONE]
  stream-bad       -> 503 before any byte
  stream-cut       -> one SSE chunk, then the connection is closed abruptly

Every POST appends the requested model to <request-log>, so call order and distribution are
proven by the upstream's own log rather than by plugin output.

POST /control with {"model": "...", "mode": "ok|400|503"} flips a model's behaviour at
runtime, which lets the proof make a member that previously succeeded start failing without
restarting the upstream.
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = sys.argv[1]
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 18342

CHUNKS = ["alpha", "bravo", "charlie"]

# Mutable per-model failure mode. "ok" serves normally; "400"/"503" override it.
MODES = {}


def append_log(model):
    with open(LOG, "a") as fh:
        fh.write(model + "\n")


def model_mode(model):
    return MODES.get(model, "ok")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _json(self, status, body):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _sse_chunk(self, model, text):
        payload = {
            "id": "chatcmpl-mock",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": model,
            "choices": [{"index": 0, "delta": {"content": text}, "finish_reason": None}],
        }
        data = "data: " + json.dumps(payload) + "\n\n"
        self.wfile.write(data.encode())
        self.wfile.flush()

    def do_GET(self):
        if self.path.endswith("/models"):
            self._json(200, {"object": "list", "data": [
                {"id": "rr-a", "object": "model"},
                {"id": "rr-b", "object": "model"},
                {"id": "rr-c", "object": "model"},
                {"id": "bad400", "object": "model"},
                {"id": "bad503", "object": "model"},
                {"id": "stream-good", "object": "model"},
                {"id": "stream-bad", "object": "model"},
                {"id": "stream-cut", "object": "model"},
            ]})
        else:
            self._json(404, {"error": "nope"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        payload = json.loads(self.rfile.read(length) or b"{}")

        if self.path.rstrip("/") == "/control":
            model = payload.get("model", "")
            mode = payload.get("mode", "ok")
            if mode not in ("ok", "400", "503"):
                self._json(400, {"error": "mode must be ok, 400 or 503"})
                return
            MODES[model] = mode
            self._json(200, {"model": model, "mode": mode})
            return

        model = payload.get("model", "")
        append_log(model)

        mode = model_mode(model)
        if mode == "503":
            self._json(503, {"error": {"message": "mock overloaded", "type": "server_error"}})
            return
        if mode == "400":
            self._json(400, {"error": {"message": "mock rejected request", "type": "invalid_request_error"}})
            return

        if model == "bad503":
            self._json(503, {"error": {"message": "mock overloaded", "type": "server_error"}})
            return
        if model == "bad400":
            self._json(400, {"error": {"message": "mock rejected request", "type": "invalid_request_error"}})
            return
        if model == "stream-bad":
            self._json(503, {"error": {"message": "mock overloaded", "type": "server_error"}})
            return
        if model == "stream-cut":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            self._sse_chunk(model, CHUNKS[0])
            time.sleep(0.1)
            self.close_connection = True
            return
        if model == "stream-good":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            for text in CHUNKS:
                self._sse_chunk(model, text)
                time.sleep(0.15)
            final = {
                "id": "chatcmpl-mock",
                "object": "chat.completion.chunk",
                "created": 1,
                "model": model,
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            }
            self.wfile.write(("data: " + json.dumps(final) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return
        if model.startswith("rr-"):
            self._json(200, {
                "id": "chatcmpl-mock",
                "object": "chat.completion",
                "created": 1,
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": "served-by:" + model},
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            })
            return
        self._json(404, {"error": {"message": "unknown model " + model}})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
