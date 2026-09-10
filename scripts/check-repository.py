#!/usr/bin/env python3
"""Check source contents without printing matched credentials or private payloads.

Default: tracked and non-ignored working files (including new files).
--staged: the complete proposed Git index, including force-added ignored files.
--tree REF: one committed tree, useful before creating a source distribution.
This is a deterministic contents check, not a complete privacy/history audit.
"""

from __future__ import annotations

import argparse
import json
import posixpath
import re
import subprocess
from pathlib import Path, PurePosixPath
from urllib.parse import unquote, urlsplit

MAX_BYTES = 2 * 1024 * 1024
PRIVATE_ROOTS = {
    "architecture", "optimization-evidence", "upstream-traces", "docs", "data",
    "local-demo", "release", "tmp", ".tmp", ".gocache", ".pnpm-store", ".claude",
}
PRIVATE_DIRS = {"verification", "test-results", "playwright-report", "node_modules", "__pycache__"}
PRIVATE_SUFFIXES = (
    ".log", ".jsonl", ".sse", ".har", ".pcap", ".pcapng", ".trace",
    ".db", ".sqlite", ".sqlite3", ".dump", ".prof", ".cpuprofile",
    ".heapprofile", ".coverprofile", ".pyc", ".test", ".tar", ".zip", ".gz",
    ".key", ".p12", ".pfx",
)
WORK_REPORT = re.compile(
    r"^(?:OPTIMIZATION_LOG|HARDENING|REASONING0_LEDGER|ACCEPTANCE|VALIDATION|"
    r"FINAL_REVIEW.*|SECOND_REVIEW.*|EXPERIMENT-.*|EXIT-.*|EGRESS-.*|ANTI_DEGRADATION_.*)\.md$",
    re.IGNORECASE,
)
SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----")),
    ("GitHub token", re.compile(rb"\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})\b")),
    ("AWS access key", re.compile(rb"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b")),
    ("Slack token", re.compile(rb"\bxox[baprs]-[A-Za-z0-9-]{24,}\b")),
    ("service secret", re.compile(rb"\b(?:sk_live_[A-Za-z0-9]{20,}|sk-proj-[A-Za-z0-9_-]{40,})\b")),
    ("JWT credential", re.compile(rb"\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{20,}\b")),
)
ASSIGNMENT = re.compile(
    rb"(?i)[\"']?(?:access[_-]?token|refresh[_-]?token|api[_-]?key|jwt[_-]?secret|"
    rb"credentialEncryptionKey|client[_-]?secret|password)[\"']?\s*[:=]\s*[\"']?"
    rb"([A-Za-z0-9_+/=-]{28,})"
)
PLACEHOLDERS = (b"example", b"placeholder", b"replace", b"change-me", b"changeme", b"test-secret", b"test-token")
EXAMPLE_SEQUENCES = (b"0123456789", b"1234567890", b"123456789", b"0123456789abcdef")


def git(root: Path, *args: str) -> bytes:
    return subprocess.check_output(["git", "-C", str(root), *args], stderr=subprocess.PIPE)


def path_problem(name: str) -> str | None:
    path = PurePosixPath(name)
    if path.parts[0] in PRIVATE_ROOTS or any(p in PRIVATE_DIRS for p in path.parts[:-1]):
        return "private records, runtime data or generated dependencies"
    if name.startswith(("backend/data/", "backend/docs/goal/", "frontend/dist/", "frontend/.cache/", ".gocache-")):
        return "runtime data or generated output"
    base = path.name
    if (base == ".env" or base.startswith(".env.") or base.endswith(".env")) and base != ".env.example":
        return "local environment file"
    if base in {"config.yaml", "config.local.yaml", "deploy.config.yaml", "id_rsa", "id_ed25519", "id_ecdsa"}:
        return "local configuration or private key"
    if base.startswith("config.") and base.endswith(".local.yaml"):
        return "local configuration"
    if base.endswith(PRIVATE_SUFFIXES) or re.search(r"\.(?:db|sqlite3?)-(?:wal|shm|journal)$", base):
        return "raw capture, database, archive or generated output"
    if WORK_REPORT.fullmatch(base):
        return "execution report; maintain durable project documentation instead"
    if name.startswith(("scripts/survey_", "scripts/trace_")):
        return "private survey tooling"
    return None


def content_problems(data: bytes) -> list[tuple[int, str]]:
    found = []
    for label, pattern in SECRET_PATTERNS:
        for match in pattern.finditer(data):
            found.append((data.count(b"\n", 0, match.start()) + 1, label))
    for match in ASSIGNMENT.finditer(data):
        value = match.group(1)
        # Repeated dummy values and visibly marked examples are not credentials.
        # This never exempts a file or bypasses the explicit token/key detectors.
        sequence = any(value == (seed * (len(value) // len(seed) + 1))[:len(value)] for seed in EXAMPLE_SEQUENCES)
        if len(set(value)) < 10 or sequence or any(marker in value.lower() for marker in PLACEHOLDERS):
            continue
        found.append((data.count(b"\n", 0, match.start()) + 1, "credential-like assignment"))
    return sorted(set(found))


def missing_markdown_links(name: str, data: bytes, available: set[str]) -> list[int]:
    """Check inline local links against candidate files, never the host filesystem.

    Directory links are supported; external URLs and page anchors are not fetched.
    Fenced code examples are not interpreted as documentation links.
    """
    missing = []
    fence = ""
    for number, line in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
        marker = re.match(r"^\s{0,3}(`{3,}|~{3,})", line)
        if marker:
            if not fence:
                fence = marker.group(1)
            elif marker.group(1)[0] == fence[0] and len(marker.group(1)) >= len(fence):
                fence = ""
            continue
        if fence:
            continue
        for match in re.finditer(r"!?\[[^\]\n]*\]\(\s*(?:<([^>\n]+)>|([^\s)]+))", line):
            destination = match.group(1) or match.group(2)
            url = urlsplit(destination)
            if url.scheme or url.netloc or not url.path:
                continue
            path = unquote(url.path)
            target = posixpath.normpath(posixpath.join(posixpath.dirname(name), path)) if not path.startswith("/") else posixpath.normpath(path.lstrip("/"))
            if target not in available:
                missing.append(number)
    return sorted(set(missing))


def index_entries(root: Path) -> list[tuple[str, str, str]]:
    entries = []
    for row in git(root, "ls-files", "--stage", "-z").split(b"\0"):
        if not row:
            continue
        metadata, name = row.split(b"\t", 1)
        mode, oid, stage = metadata.decode("ascii").split()
        if stage != "0":
            raise ValueError("unmerged index entries must be resolved first")
        entries.append((name.decode("utf-8", "surrogateescape"), mode, oid))
    return entries


def tree_entries(root: Path, ref: str) -> list[tuple[str, str, str]]:
    # Resolve once and use only the resulting object ID as the tree argument.
    tree = git(root, "rev-parse", "--verify", "--end-of-options", ref + "^{tree}").strip().decode("ascii")
    entries = []
    for row in git(root, "ls-tree", "-r", "-z", tree).split(b"\0"):
        if row:
            metadata, name = row.split(b"\t", 1)
            mode, _, oid = metadata.decode("ascii").split()
            entries.append((name.decode("utf-8", "surrogateescape"), mode, oid))
    return entries


def inspect(root: Path, entries: list[tuple[str, str, str]], working: bool) -> tuple[int, list[str]]:
    problems = []
    checked = 0
    available = {"."}
    documents = []
    batch = None if working else subprocess.Popen(
        ["git", "-C", str(root), "cat-file", "--batch"],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
    )
    try:
        for name, mode, oid in entries:
            label = json.dumps(name, ensure_ascii=True)
            if working:
                file = root / name
                if file.is_symlink():
                    problems.append(f"{label}: symlinks require explicit source packaging review")
                    continue
                if not file.exists():
                    continue  # A tracked deletion in the proposed working tree.
                if not file.is_file():
                    problems.append(f"{label}: unsupported non-file entry")
                    continue
                size = file.stat().st_size
                data = file.read_bytes() if size <= MAX_BYTES else b""
            else:
                if mode not in {"100644", "100755"}:
                    problems.append(f"{label}: symlinks/submodules require explicit source packaging review")
                    continue
                batch.stdin.write(oid.encode("ascii") + b"\n")
                batch.stdin.flush()
                header = batch.stdout.readline().split()
                if len(header) != 3 or header[1] != b"blob":
                    raise ValueError("unable to read indexed blob")
                size = int(header[2])
                if size <= MAX_BYTES:
                    data = batch.stdout.read(size)
                else:
                    remaining = size
                    while remaining:
                        chunk = batch.stdout.read(min(remaining, 65536))
                        if not chunk:
                            raise ValueError("truncated Git object")
                        remaining -= len(chunk)
                    data = b""
                if batch.stdout.read(1) != b"\n":
                    raise ValueError("invalid Git object framing")
            checked += 1
            available.add(name)
            available.update(str(parent) for parent in PurePosixPath(name).parents)
            if name.lower().endswith(".md"):
                documents.append((name, data))
            reason = path_problem(name)
            if reason:
                problems.append(f"{label}: {reason}")
            if size > MAX_BYTES:
                problems.append(f"{label}: file exceeds the 2 MiB source limit; review its purpose before changing the policy")
            for line, reason in content_problems(data):
                problems.append(f"{label}:{line}: {reason} (value redacted)")
    finally:
        if batch is not None:
            batch.stdin.close()
            batch.stdout.close()
            batch.wait()
    for name, data in documents:
        for line in missing_markdown_links(name, data, available):
            problems.append(f"{json.dumps(name, ensure_ascii=True)}:{line}: local Markdown link target is absent from the candidate source")
    return checked, problems


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--staged", action="store_true", help="inspect every file in the proposed Git index")
    mode.add_argument("--tree", metavar="REF", help="inspect a committed tree (not its ancestors)")
    args = parser.parse_args()
    try:
        root = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], stderr=subprocess.PIPE).decode().strip())
        working = not (args.staged or args.tree)
        if args.tree:
            entries = tree_entries(root, args.tree)
        elif args.staged:
            entries = index_entries(root)
        else:
            names = git(root, "ls-files", "--cached", "--others", "--exclude-standard", "-z").split(b"\0")
            entries = [(n.decode("utf-8", "surrogateescape"), "", "") for n in sorted(set(names)) if n]
        count, problems = inspect(root, entries, working)
    except (OSError, ValueError, subprocess.CalledProcessError):
        # Subprocess diagnostics may contain remote URLs or private file content.
        print("Repository check could not inspect the source tree/index; check Git state and file access.")
        return 2
    if problems:
        for problem in problems[:40]:
            print(problem)
        if len(problems) > 40:
            print(f"... {len(problems) - 40} additional findings omitted")
        print(f"Repository check FAILED: {len(problems)} findings in {count} files. See DEVELOPMENT.md.")
        return 1
    print(f"Repository check PASS: {count} files checked; manual privacy and history review still required.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
