#!/usr/bin/env python3
"""Core API stand-in for the mobile simulator E2E (no PostgreSQL / codecs needed).

Implements the parts of POST /media the mobile clients depend on, with the
server's idempotency semantics (internal/upload/repository.go):
  - new key, new content             -> 201 {"media":{"id":...},"job":null}
  - same key, same content+filename  -> stored response replayed + Idempotency-Replayed: true
  - same key, different content/name -> 409 idempotency_conflict
  - new key, content already stored  -> 409 duplicate_media {details.existing_media_id}
It parses the multipart body strictly (exactly one part named "file", RFC 8187
filename*), stores each original under --store/<sha256>, and appends one JSON
line per request to --log.

Fault injection: --drop-first N stores the first N new uploads but closes the
connection without answering, like a response lost on the network; the client
must retry with the same key and receive the replay.
"""
import argparse
import hashlib
import json
import os
import re
import threading
import unicodedata
import urllib.parse
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()
KEYS = {}  # key -> (request_hash, status, body)
SHAS = {}  # content sha -> media id
STATE = {"drops_left": 0}


def parse_upload(content_type, body):
    m = re.fullmatch(r'multipart/form-data; boundary=([A-Za-z0-9\'()+_,\-./:=? ]{1,70})', content_type or "")
    if not m:
        raise ValueError("bad Content-Type: %r" % content_type)
    boundary = m.group(1).encode()
    if not body.startswith(b"--" + boundary + b"\r\n"):
        raise ValueError("body does not start with boundary")
    tail = b"\r\n--" + boundary + b"--\r\n"
    if not body.endswith(tail):
        raise ValueError("body does not end with closing boundary")
    inner = body[len(boundary) + 4:-len(tail)]
    head, sep, payload = inner.partition(b"\r\n\r\n")
    if not sep:
        raise ValueError("no part header terminator")
    if (b"\r\n--" + boundary) in payload:
        raise ValueError("more than one part")
    headers = {}
    for line in head.decode("utf-8").split("\r\n"):
        k, _, v = line.partition(":")
        headers[k.strip().lower()] = v.strip()
    disp = headers.get("content-disposition", "")
    if not disp.startswith('form-data; name="file"'):
        raise ValueError("part is not name=file: %r" % disp)
    filename = None
    star = re.search(r"filename\*=UTF-8''([^;\s]+)", disp)
    if star:
        filename = urllib.parse.unquote(star.group(1), encoding="utf-8", errors="strict")
    else:
        plain = re.search(r'filename="([^"]*)"', disp)
        filename = plain.group(1) if plain else None
    if filename:
        filename = unicodedata.normalize("NFC", filename.replace("\\", "/").split("/")[-1])
    return payload, filename, headers.get("content-type")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _json(self, status, obj, extra=None):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(data)

    def _record(self, **entry):
        with LOCK, open(self.server.args.log, "a") as f:
            f.write(json.dumps(entry, ensure_ascii=False) + "\n")

    def do_GET(self):
        if self.path in ("/readyz", "/healthz"):
            self._json(200, {"status": "ok"})
        else:
            self._json(404, {"error": {"code": "not_found", "message": "route not found"}})

    def do_POST(self):
        if self.path != "/media":
            return self._json(404, {"error": {"code": "not_found", "message": "route not found"}})
        key = self.headers.get("Idempotency-Key")
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        try:
            if not key or not re.fullmatch(r"[!-~]{1,128}", key):
                raise ValueError("invalid idempotency key %r" % key)
            payload, filename, part_type = parse_upload(self.headers.get("Content-Type"), body)
        except (ValueError, UnicodeDecodeError) as e:
            self._record(event="rejected", key=key, error=str(e))
            return self._json(400, {"error": {"code": "invalid_multipart", "message": str(e)}})
        sha = hashlib.sha256(payload).hexdigest()
        request_hash = hashlib.sha256(f"{sha}|{len(payload)}|{filename}".encode()).hexdigest()
        drop = False
        with LOCK:
            stored = KEYS.get(key)
            if stored:
                if stored[0] != request_hash:
                    status, resp, replayed = 409, {"error": {"code": "idempotency_conflict", "message": "idempotency key was used with different content", "details": {}}}, False
                else:
                    status, resp, replayed = stored[1], stored[2], True
            elif sha in SHAS:
                status, resp, replayed = 409, {"error": {"code": "duplicate_media", "message": "original content already exists", "details": {"existing_media_id": SHAS[sha], "deleted": False}}}, False
                KEYS[key] = (request_hash, status, resp)
            else:
                media_id = str(uuid.uuid4())
                SHAS[sha] = media_id
                path = os.path.join(self.server.args.store, sha)
                with open(path, "wb") as f:
                    f.write(payload)
                status, resp, replayed = 201, {"media": {"id": media_id, "original_filename": filename}, "job": None}, False
                KEYS[key] = (request_hash, status, resp)
                if STATE["drops_left"] > 0:
                    STATE["drops_left"] -= 1
                    drop = True
        self._record(event="upload", key=key, filename=filename, part_type=part_type, sha256=sha,
                     size=len(payload), status=status, replayed=replayed, dropped=drop,
                     code=(resp.get("error") or {}).get("code"))
        if drop:
            # Stored, but the client never learns: simulate a lost response.
            self.close_connection = True
            try:
                self.connection.shutdown(2)
            except OSError:
                pass
            return
        self._json(status, resp, {"Idempotency-Replayed": "true"} if replayed else None)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, default=8787)
    p.add_argument("--store", required=True)
    p.add_argument("--log", required=True)
    p.add_argument("--drop-first", type=int, default=0)
    args = p.parse_args()
    os.makedirs(args.store, exist_ok=True)
    STATE["drops_left"] = args.drop_first
    server = ThreadingHTTPServer(("0.0.0.0", args.port), Handler)
    server.args = args
    server.serve_forever()


if __name__ == "__main__":
    main()
