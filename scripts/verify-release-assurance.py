#!/usr/bin/env python3
"""Bind successful reusable CI gates to exact committed release source bytes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess

REQUIRED = {
    "go", "go-race", "govulncheck", "staticcheck", "tracker-contracts", "ui-unit",
    "sdk-go", "sdk-browser", "sdk-python", "sdk-sentry-shim", "ui-freshness",
    "vendor-pin-parity", "replay-freshness", "nucleus-integration",
    "nucleus-integration-arm64", "nucleus-lease", "e2e-smoke", "helm-pair-availability",
}


def identity(root):
    sha = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
    if subprocess.run(["git", "diff", "--quiet", "HEAD", "--"], cwd=root).returncode != 0:
        raise ValueError("release checkout contains uncommitted source changes")
    entries = []
    for item in subprocess.check_output(["git", "ls-files", "-s", "-z"], cwd=root).split(b"\0"):
        if not item:
            continue
        header, path = item.split(b"\t", 1)
        mode, blob, stage = header.decode().split()
        if stage != "0":
            raise ValueError("unmerged release source")
        name = os.fsdecode(path)
        if mode == "160000":
            entries.append([name, mode, blob])
            continue
        target = root / name
        data = os.fsencode(os.readlink(target)) if mode == "120000" else target.read_bytes()
        entries.append([name, mode, hashlib.sha256(data).hexdigest()])
    return sha, hashlib.sha256(json.dumps(entries, separators=(",", ":")).encode()).hexdigest()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--record", action="store_true")
    parser.add_argument("receipt", type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    sha, digest = identity(root)
    expected = os.environ["GITHUB_SHA"]
    if sha != expected:
        raise ValueError("checkout differs from triggering SHA")
    if args.record:
        gates = json.loads(os.environ["GATE_RESULTS"])
        if set(gates) != REQUIRED or any(g["result"] != "success" for g in gates.values()):
            raise ValueError("required CI gate absent, skipped or unsuccessful")
        receipt = {"schema": 1, "sha": sha, "source_sha256": digest,
                   "run_id": os.environ["GITHUB_RUN_ID"], "run_attempt": os.environ["GITHUB_RUN_ATTEMPT"],
                   "gates": {name: gates[name]["result"] for name in sorted(REQUIRED)}}
        args.receipt.write_text(json.dumps(receipt, indent=2) + "\n")
    else:
        receipt = json.loads(args.receipt.read_text())
        if receipt.get("schema") != 1 or receipt.get("sha") != sha or receipt.get("source_sha256") != digest:
            raise ValueError("assurance receipt source mismatch")
        if receipt.get("run_id") != os.environ["GITHUB_RUN_ID"] or receipt.get("run_attempt") != os.environ["GITHUB_RUN_ATTEMPT"]:
            raise ValueError("assurance receipt is from another run/attempt")
        if set(receipt.get("gates", {})) != REQUIRED or set(receipt["gates"].values()) != {"success"}:
            raise ValueError("assurance gates incomplete")
    print(f"Exact-SHA assurance verified: {sha}")


if __name__ == "__main__":
    main()
