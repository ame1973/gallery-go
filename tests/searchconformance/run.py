#!/usr/bin/env python3
"""Compare the pinned Go and browser search profiles using isolated test data."""

import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]


def run(label, command, *, env, stdout=None):
    print(f"[START] {label}", flush=True)
    subprocess.run(command, cwd=ROOT, env=env, stdout=stdout, check=True)
    print(f"[PASS] {label}", flush=True)


def main():
    directory = Path(tempfile.mkdtemp(prefix="gallerygo-search-conformance-"))
    env = os.environ.copy()
    env["GOTOOLCHAIN"] = "local"
    env["CGO_ENABLED"] = "0"
    corpus = directory / "corpus.json"
    try:
        run(
            "Verify generated Unicode profile",
            ["go", "run", "./internal/search/cmd/unicodegen", "-check"],
            env=env,
        )
        run(
            "Go search fixtures",
            ["go", "test", "-p=1", "-count=1", "./internal/search", "./tests/searchconformance"],
            env=env,
        )
        with corpus.open("xb") as output:
            run(
                "Generate deterministic Go oracle",
                [
                    "go",
                    "run",
                    "./tests/searchconformance",
                    "contracts/search-normalization-tables.json",
                    "tests/fixtures/unicode15-normalization.txt.gz",
                    "tests/fixtures/search-long-unicode15.json.gz",
                ],
                env=env,
                stdout=output,
            )
        env["GALLERYGO_SEARCH_CORPUS"] = str(corpus)
        env["GALLERYGO_SEARCH_SCALAR_CHECK"] = "1"
        run(
            "Node search fixtures and differential checks",
            [
                "pnpm",
                "--dir",
                "web",
                "exec",
                "vitest",
                "run",
                "src/lib/search.test.ts",
                "src/lib/search-scalar.test.ts",
                "src/lib/search-differential.test.ts",
            ],
            env=env,
        )
        run(
            "Chromium search conformance",
            [
                "pnpm",
                "exec",
                "playwright",
                "test",
                "--config",
                "playwright.search.config.ts",
                "--output",
                str(directory / "results"),
            ],
            env=env,
        )
    except (OSError, subprocess.CalledProcessError):
        print(f"Search conformance failed; temporary artifacts: {directory}", file=sys.stderr)
        raise
    else:
        shutil.rmtree(directory)


if __name__ == "__main__":
    main()
