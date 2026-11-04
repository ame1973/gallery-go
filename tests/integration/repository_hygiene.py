#!/usr/bin/env python3
"""Reject private tracked files and recognizable credential literals in source."""
from pathlib import Path
import re
import subprocess
import sys

root = Path(__file__).resolve().parents[2]
tracked = subprocess.check_output(["git", "ls-files", "-z"], cwd=root).decode().split("\0")
violations = []
patterns = [
    re.compile(rb"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
    re.compile(rb"\bAKIA[0-9A-Z]{16}\b"),
    re.compile(rb"\bgh[pousr]_[A-Za-z0-9]{30,}\b"),
    re.compile(rb"\bgithub_pat_[A-Za-z0-9_]{40,}\b"),
]
for name in filter(None, tracked):
    path = Path(name)
    private = (
        path.parts[0] in {"dev", ".local", ".claude", "plans", "data", "cache", "media", "backups", "logs"}
        or path.name.lower().startswith("claude") and path.suffix == ".md"
        or path.name == "AGENTS.md"
        or path.name.startswith(".env") and path.name != ".env.example"
        or path.name == "config.yaml"
        or path.suffix.lower() in {".pem", ".key", ".p12", ".db", ".sqlite"}
        or name.startswith("docs/adr/")
    )
    if private:
        violations.append(f"private file is tracked: {name}")
    data = (root / name).read_bytes()
    if any(pattern.search(data) for pattern in patterns):
        violations.append(f"possible credential in tracked file: {name}")
if violations:
    print("\n".join(violations), file=sys.stderr)
    sys.exit(1)
print("PASS: tracked-file exclusions and credential-literal checks")
