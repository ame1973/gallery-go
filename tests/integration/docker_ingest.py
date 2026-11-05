#!/usr/bin/env python3
"""Exercise native indexed ingestion in a rootless, read-only, ephemeral image."""
import hashlib
import http.client
import http.cookiejar
import http.server
import json
from pathlib import Path
import secrets
import ssl
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid
import zlib


def docker(*args, input=None):
    result = subprocess.run(["docker", *args], input=input, text=True,
                            capture_output=True, timeout=60, check=False)
    if result.returncode != 0:
        raise RuntimeError(f"Docker operation {args[0]} failed: {result.stderr}")
    return (result.stdout + result.stderr if args[0] == "logs" else result.stdout).strip()


def png(width, height):
    def chunk(kind, data):
        return struct.pack("!I", len(data)) + kind + data + struct.pack("!I", zlib.crc32(kind + data))
    pixels = b"".join(b"\x00" + b"\x20\x60\x90\xff" * width for _ in range(height))
    return (b"\x89PNG\r\n\x1a\n" +
            chunk(b"IHDR", struct.pack("!IIBBBBB", width, height, 8, 6, 0, 0, 0)) +
            chunk(b"IDAT", zlib.compress(pixels)) + chunk(b"IEND", b""))


class Proxy(http.server.BaseHTTPRequestHandler):
    # This TLS reverse proxy is a test client boundary, not a product endpoint.
    def forward(self):
        connection = http.client.HTTPConnection("127.0.0.1", self.server.backend_port, timeout=10)
        excluded = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
                    "te", "trailer", "transfer-encoding", "upgrade", "host"}
        try:
            body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            headers = {name: value for name, value in self.headers.items() if name.lower() not in excluded}
            connection.request(self.command, self.path, body=body, headers=headers)
            response = connection.getresponse()
            data = response.read()
            self.send_response(response.status)
            for name, value in response.getheaders():
                if name.lower() not in excluded | {"content-length"}:
                    self.send_header(name, value)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(data)
        finally:
            connection.close()

    do_GET = do_POST = do_PATCH = do_PUT = do_DELETE = do_HEAD = forward

    def log_message(self, *_args):
        pass


class API:
    def __init__(self, origin, certificate):
        self.origin, self.csrf = origin, ""
        context = ssl.create_default_context(cafile=str(certificate))
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPSHandler(context=context),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
        )

    def request(self, method, path, body=None, status=200, headers=None, raw=False):
        fields = dict(headers or {})
        data = None if body is None else json.dumps(body).encode()
        if data is not None:
            fields["Content-Type"] = "application/json"
        if method not in ("GET", "HEAD"):
            fields.update({"Origin": self.origin, "Idempotency-Key": str(uuid.uuid4())})
            if self.csrf:
                fields["X-CSRF-Token"] = self.csrf
        request = urllib.request.Request(self.origin + "/api/v2" + path, data, fields, method=method)
        try:
            response = self.opener.open(request, timeout=10)
        except urllib.error.HTTPError as failure:
            response = failure
        with response:
            result = response.read()
            assert response.status == status, (method, path, response.status, status, result)
            assert response.headers["Cache-Control"] == "private, no-store"
            return (result if raw else json.loads(result or b"null")), response.headers

    def scan(self, source, mode="full", expected="succeeded"):
        job, _ = self.request("POST", "/admin/scans", {"mode": mode, "source_id": source}, status=202)
        deadline = time.monotonic() + 30
        while True:
            current, _ = self.request("GET", "/admin/jobs/" + job["id"])
            if current["status"] not in ("queued", "running"):
                assert current["status"] == expected, current
                assert current["done"] == sum(current[key] for key in ("succeeded", "failed", "skipped", "cancelled"))
                return current
            assert time.monotonic() < deadline, current
            time.sleep(0.05)


def run(image, directory, embedded):
    source = directory / "source"
    album = source / "Album"
    album.mkdir(parents=True)
    marker = source / ".mounted"
    marker.write_bytes(b"isolated container source marker")
    inputs = {"first.png": png(12, 7), "second.png": png(13, 7)}
    for name, data in inputs.items():
        (album / name).write_bytes(data)
        (album / name).chmod(0o444)
    marker.chmod(0o444)
    album.chmod(0o555)
    source.chmod(0o555)
    certificate, key = directory / "tls.pem", directory / "tls.key"
    subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
                    "-nodes", "-keyout", str(key), "-out", str(certificate), "-days", "1",
                    "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1"],
                   check=True, capture_output=True, timeout=30)
    proxy = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Proxy)
    proxy.daemon_threads = True
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(certificate, key)
    proxy.socket = context.wrap_socket(proxy.socket, server_side=True)
    origin = f"https://127.0.0.1:{proxy.server_port}"
    config = directory / "config.yaml"
    config.write_text(json.dumps({
        "listen": "0.0.0.0:8080", "base_url": origin, "database": "/data/gallerygo.db",
        "cache_root": "/cache", "web_root": "/app/web",
        "sources": [{"name": "fixture", "root": "/source", "marker": ".mounted"}],
        "worker": {"embedded": embedded, "concurrency": 1, "poll": "100ms", "max_attempts": 2,
                   "retry_base": "100ms", "retry_max": "200ms"},
        "scheduler": {"enabled": False},
    }))
    config.chmod(0o444)  # Explicit read-only bind; no credentials are stored here.
    container, thread = None, None
    try:
        container = docker(
            "run", "--detach", "--rm", "--read-only", "--user", "65532:65532",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
            "--pids-limit", "128", "--memory", "512m", "--cpus", "2",
            "--tmpfs", "/data:uid=65532,gid=65532,mode=0700",
            "--tmpfs", "/cache:uid=65532,gid=65532,mode=0700",
            "--tmpfs", "/tmp:noexec,nosuid,size=64m",
            "--mount", f"type=bind,src={source},dst=/source,readonly",
            "--mount", f"type=bind,src={config},dst=/app/config/integration.yaml,readonly",
            "--publish", "127.0.0.1::8080", "--entrypoint", "/bin/sh", image, "-ec",
            "gallerygo migrate --config /app/config/integration.yaml && "
            "exec gallerygo serve --config /app/config/integration.yaml",
        )
        info = json.loads(docker("inspect", container))[0]
        assert info["Config"]["User"] == "65532:65532"
        assert info["HostConfig"]["ReadonlyRootfs"] is True
        assert info["HostConfig"]["CapDrop"] == ["ALL"]
        assert any(mount["Destination"] == "/source" and mount["RW"] is False for mount in info["Mounts"])
        proxy.backend_port = int(info["NetworkSettings"]["Ports"]["8080/tcp"][0]["HostPort"])
        probe = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        live_deadline = time.monotonic() + 30
        while True:
            try:
                with probe.open(f"http://127.0.0.1:{proxy.backend_port}/api/v2/health/live", timeout=2) as response:
                    assert response.status == 200
                break
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= live_deadline:
                    raise
                time.sleep(0.1)
        if not embedded:
            docker("exec", "--detach", container, "/bin/sh", "-ec",
                   "umask 077; exec gallerygo worker --config /app/config/integration.yaml >/tmp/worker.log 2>&1")
        thread = threading.Thread(target=proxy.serve_forever, daemon=True)
        thread.start()
        api = API(origin, certificate)
        deadline = time.monotonic() + 30
        while True:
            try:
                ready, _ = api.request("GET", "/health/ready")
                assert ready == {"status": "ok", "database": "ok", "worker": "ok", "sources": "ok"}
                break
            except (OSError, urllib.error.URLError, AssertionError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.1)
        secret = secrets.token_urlsafe(32)
        docker("exec", "-i", container, "gallerygo", "bootstrap-admin", "--config",
               "/app/config/integration.yaml", "--username", "owner", "--password-stdin", input=secret + "\n")
        api.request("POST", "/auth/login", {"username": "owner", "password": secret})
        token, _ = api.request("GET", "/auth/csrf")
        api.csrf = token["token"]
        library, _ = api.request("POST", "/admin/libraries", {"name": "Isolated container ingestion"}, status=201)
        registration, _ = api.request("POST", "/admin/sources", {
            "library_id": library["id"], "root_name": "fixture", "minimum_files": 2,
        }, status=201)
        initial = api.scan(registration["id"])
        assert initial["attempts"] == 1 and initial["succeeded"] == 6 and initial["total_known"] is True, initial
        albums, _ = api.request("GET", "/albums")
        assert len(albums["items"]) == 1, albums
        album_id = albums["items"][0]["id"]
        media, _ = api.request("GET", f"/albums/{album_id}/media")
        assert len(media["items"]) == 2, media
        identities = {item["file_name"]: (item["id"], item["revision"]) for item in media["items"]}
        assert {item["width"] for item in media["items"]} == {12, 13}
        anonymous = API(origin, certificate)
        anonymous.request("GET", f"/media/{next(iter(identities.values()))[0]}/original", status=401)
        for name, (media_id, _revision) in identities.items():
            original, headers = api.request("GET", f"/media/{media_id}/original", raw=True)
            assert original == inputs[name] and headers["Content-Type"] == "image/png"
            part, headers = api.request("GET", f"/media/{media_id}/original", status=206,
                                        headers={"Range": "bytes=0-3"}, raw=True)
            assert part == inputs[name][:4] and headers["Content-Range"] == f"bytes 0-3/{len(inputs[name])}"
        api.scan(registration["id"], "incremental")
        media, _ = api.request("GET", f"/albums/{album_id}/media")
        assert {item["file_name"]: (item["id"], item["revision"]) for item in media["items"]} == identities
        source.chmod(0o755)
        album.rename(source / "Renamed")
        source.chmod(0o555)
        api.scan(registration["id"])
        albums, _ = api.request("GET", "/albums")
        assert len(albums["items"]) == 1 and albums["items"][0]["id"] == album_id, albums
        media, _ = api.request("GET", f"/albums/{album_id}/media")
        assert {item["file_name"]: (item["id"], item["revision"]) for item in media["items"]} == identities
        healthy, _ = api.request("GET", "/admin/sources/" + registration["id"])
        source.chmod(0o755)
        marker.unlink()
        source.chmod(0o555)
        failed = api.scan(registration["id"], expected="dead")
        assert failed["attempts"] == 2 and failed["failed"] == 2, failed
        unavailable, _ = api.request("GET", "/admin/sources/" + registration["id"])
        assert unavailable["state"] == "unavailable" and unavailable["scan_generation"] == healthy["scan_generation"]
        media, _ = api.request("GET", f"/albums/{album_id}/media")
        assert {item["file_name"]: (item["id"], item["revision"]) for item in media["items"]} == identities
        api.request("GET", f"/media/{next(iter(identities.values()))[0]}/original", status=503)
        ready, _ = api.request("GET", "/health/ready")
        assert ready["status"] == "degraded" and ready["worker"] == "ok", ready
        assert not marker.exists(), "Container recreated a missing source marker"
        source.chmod(0o755)
        marker.write_bytes(b"restored isolated fixture marker")
        marker.chmod(0o444)
        source.chmod(0o555)
        api.scan(registration["id"])
        media, _ = api.request("GET", f"/albums/{album_id}/media")
        assert {item["file_name"]: (item["id"], item["revision"]) for item in media["items"]} == identities
        for name, expected in inputs.items():
            assert hashlib.sha256((source / "Renamed" / name).read_bytes()).digest() == hashlib.sha256(expected).digest()
        denied = subprocess.run(["docker", "exec", container, "/bin/sh", "-ec",
                                 "touch /source/must-not-be-created"], capture_output=True, timeout=10, check=False)
        assert denied.returncode != 0 and not (source / "must-not-be-created").exists(), "Source mount was writable"
        doctor = json.loads(docker("exec", container, "gallerygo", "doctor", "--config", "/app/config/integration.yaml"))
        assert doctor["database"] == "ok" and doctor["schema"] == "ok"
        for options in [(), ("--worker",)]:
            state = json.loads(docker("exec", container, "gallerygo", "healthcheck", *options,
                                      "--config", "/app/config/integration.yaml"))
            assert state == {"status": "ok", "database": "ok", "worker": "ok", "sources": "ok"}
        mode = "embedded" if embedded else "standalone"
        print(f"PASS ({mode}): rootless/read-only native ingestion, SQLite publication plans, private originals/Range, "
              "stable rename/revision IDs, outage retention/recovery, read-only source and dependency probes")
    except BaseException:
        if container:
            print(docker("logs", container), file=sys.stderr)
        raise
    finally:
        if thread:
            proxy.shutdown()
            thread.join(timeout=5)
        proxy.server_close()
        if container:
            # Only this run's ID is stopped; no Compose volume or existing data is used.
            docker("stop", "--time", "10", container)
        for path in source.rglob("*"):
            if path.is_dir():
                path.chmod(0o755)
        source.chmod(0o755)


def main():
    image = sys.argv[1] if len(sys.argv) > 1 else "gallerygo:local"
    for embedded in (True, False):
        with tempfile.TemporaryDirectory(prefix="gallerygo-container-ingest-") as temporary:
            run(image, Path(temporary), embedded)


if __name__ == "__main__":
    main()
