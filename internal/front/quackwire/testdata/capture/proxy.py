#!/usr/bin/env python3
"""The corpus's capture proxy (spec 002): forwards POST /quack to one node and keeps every message it sees.

Usage: proxy.py <port> <node host:port> <out dir>. Each request and answer body is written to <out dir> as
<seq>-<request|answer>-<type>.bin, <type> the message's first header field (duckdb's BinarySerializer: field 1 is
a u16 1, then the type as a varint). After the client's DISCONNECT the proxy replays the last PREPARE it saw, so the
corpus has the node's answer to a connection it no longer knows.
"""
import http.client
import http.server
import os
import socketserver
import sys
import threading

PORT, NODE, OUT = int(sys.argv[1]), sys.argv[2], sys.argv[3]
LOCK = threading.Lock()
STATE = {"seq": 0, "prepare": None, "headers": None}
HOP = {"host", "content-length", "connection", "transfer-encoding", "keep-alive"}


def kind(body):
    return body[2] if len(body) > 2 and body[0:2] == b"\x01\x00" else 0


def keep(direction, body):
    with LOCK:
        STATE["seq"] += 1
        name = "%03d-%s-%d.bin" % (STATE["seq"], direction, kind(body))
    with open(os.path.join(OUT, name), "wb") as f:
        f.write(body)


def forward(path, headers, body):
    host, port = NODE.rsplit(":", 1)
    conn = http.client.HTTPConnection(host, int(port), timeout=120)
    conn.request("POST", path, body=body, headers=headers)
    answer = conn.getresponse()
    data = answer.read()
    out = [(n, v) for n, v in answer.getheaders() if n.lower() not in HOP]
    conn.close()
    return answer.status, out, data


class Proxy(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        headers = {n: v for n, v in self.headers.items() if n.lower() not in HOP}
        keep("request", body)
        status, out, data = forward(self.path, headers, body)
        keep("answer", data)
        if kind(body) == 3:
            with LOCK:
                STATE["prepare"], STATE["headers"] = body, headers
        if kind(body) == 11 and STATE["prepare"] is not None:
            _, _, stale = forward(self.path, STATE["headers"], STATE["prepare"])
            keep("answer-stale", stale)
        self.send_response(status)
        for name, value in out:
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


Server(("127.0.0.1", PORT), Proxy).serve_forever()
