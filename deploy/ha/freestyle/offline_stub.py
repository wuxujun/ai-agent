#!/usr/bin/env python3
"""Bounded offline fixture for read_file planner/writer calls, not a real model."""
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ipaddress
import json
import os


def response_for(schema):
    properties = schema.get("properties", {})
    if set(properties) == {"thought_summary", "steps"}:
        item = properties["steps"]["items"]
        if item["properties"]["action"].get("enum") != ["read_file"]:
            raise ValueError("only the dedicated read_file team is supported")
        step = {key: 0 if value.get("type") == "integer" else "" for key, value in item["properties"].items()}
        step.update(id="step-1", description="Read the shared HA fixture", action="read_file", file_path="README.md")
        return {"thought_summary": "Read the dedicated shared fixture.", "steps": [step]}
    if set(properties) == {"final_answer", "evidence_summary", "draft_confidence"}:
        return {"final_answer": "The shared README describes an isolated AI Agent HA test fixture.",
            "evidence_summary": "README.md is the fixed local fixture.", "draft_confidence": "high"}
    raise ValueError("unsupported schema; use a reviewed fixture for other scenarios")


class Handler(BaseHTTPRequestHandler):
    token = ""
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass  # Never log headers, paths, prompts, response bodies or secrets.

    def reply(self, code, value):
        body = json.dumps(value).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        self.connection.settimeout(15)
        if not hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer " + self.token):
            self.close_connection = True
            self.reply(401, {"error": "unauthorized"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= 1024 * 1024:
                raise ValueError()
            body = json.loads(self.rfile.read(length))
            if not isinstance(body, dict):
                raise ValueError()
            if self.path == "/v1/embeddings":
                value = body.get("input", "")
                count = len(value) if isinstance(value, list) else 1
                if count > 64:
                    raise ValueError()
                self.reply(200, {"object": "list", "data": [{"object": "embedding", "index": i,
                    "embedding": [1.0, 0.0, 0.0]} for i in range(count)], "model": "ha-embedding",
                    "usage": {"prompt_tokens": 1, "total_tokens": 1}})
                return
            if self.path != "/v1/chat/completions":
                raise ValueError()
            result = response_for(body["response_format"]["json_schema"]["schema"])
            content = json.dumps(result)
            usage = {"prompt_tokens": 20, "completion_tokens": 30, "total_tokens": 50}
            if body.get("stream"):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()
                chunk = {"choices": [{"index": 0, "delta": {"content": content}}], "usage": usage}
                self.wfile.write(("data: " + json.dumps(chunk) + "\n\ndata: [DONE]\n\n").encode())
                self.wfile.flush()
                self.close_connection = True
            else:
                self.reply(200, {"choices": [{"index": 0, "message": {"role": "assistant", "content": content},
                    "finish_reason": "stop"}], "usage": usage})
        except (ValueError, KeyError, TypeError):
            self.close_connection = True
            self.reply(400, {"error": "unsupported offline fixture request"})


if __name__ == "__main__":
    address = ipaddress.IPv4Address(os.environ["HA_STUB_ADDR"])
    if not address.is_private or address.is_unspecified or not os.environ.get("HA_STUB_TOKEN"):
        raise SystemExit("A private address and token are required")
    Handler.token = os.environ["HA_STUB_TOKEN"]
    server = ThreadingHTTPServer((str(address), 18080), Handler)
    server.daemon_threads = True
    server.serve_forever()
