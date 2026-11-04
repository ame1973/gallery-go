#!/usr/bin/env python3
"""Exercise a built image using only private, ephemeral container storage."""
import json
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def main():
    image = sys.argv[1] if len(sys.argv) > 1 else "gallerygo:local"
    container = docker(
        "run", "--detach", "--rm", "--read-only", "--user", "65532:65532",
        "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
        "--pids-limit", "128", "--memory", "512m", "--cpus", "2",
        "--tmpfs", "/data:uid=65532,gid=65532,mode=0700",
        "--tmpfs", "/cache:uid=65532,gid=65532,mode=0700",
        "--tmpfs", "/tmp:noexec,nosuid,size=64m",
        "--publish", "127.0.0.1::8080", "--entrypoint", "/bin/sh", image, "-ec",
        "gallerygo migrate --config /app/config/config.yaml && "
        "exec gallerygo serve --config /app/config/config.yaml",
    )
    try:
        info = json.loads(docker("inspect", container))[0]
        assert info["Config"]["User"] == "65532:65532"
        assert info["HostConfig"]["ReadonlyRootfs"] is True
        assert info["HostConfig"]["CapDrop"] == ["ALL"]
        port = info["NetworkSettings"]["Ports"]["8080/tcp"][0]["HostPort"]
        base = f"http://127.0.0.1:{port}"
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        deadline = time.monotonic() + 30
        while True:
            try:
                with opener.open(base + "/api/v2/health/ready", timeout=2) as response:
                    assert json.load(response) == {"status": "ok"}
                    assert response.headers["Cache-Control"] == "private, no-store"
                break
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.1)
        with opener.open(base, timeout=3) as response:
            html = response.read().decode()
        assets = re.findall(r'(?:src|href)="(/assets/[^\"]+)"', html)
        assert len(assets) >= 2, "Production JS and CSS are missing"
        for path in [*assets, "/favicon.svg"]:
            with opener.open(base + path, timeout=3) as response:
                assert response.status == 200
                assert response.headers["X-Content-Type-Options"] == "nosniff"
                assert len(response.read()) > 0
        result = json.loads(docker("exec", container, "gallerygo", "doctor", "--config", "/app/config/config.yaml"))
        assert result["database"] == "ok" and result["schema"] == "ok"
        docker("exec", container, "gallerygo", "healthcheck", "--config", "/app/config/config.yaml")
        print("PASS: rootless/read-only container, migrations, HTTP, assets, doctor, healthcheck")
    except BaseException:
        print(docker("logs", container), file=sys.stderr)
        raise
    finally:
        # The ID came from this test's docker run; no user container or volume is touched.
        docker("stop", "--time", "10", container)


if __name__ == "__main__":
    main()
