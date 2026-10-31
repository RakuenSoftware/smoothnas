#!/usr/bin/env python3
"""Validate a PBS plugin datastore and exercise a disposable backup group.

Credentials use the normal PBS_* environment variables. Run --restore-only
again after PBS verification/GC, tier movement, or plugin reinstall.
"""

import argparse
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid


def filesystem_probe(path):
    # Use a new sibling directory, never modify actual PBS chunks or indexes.
    with tempfile.TemporaryDirectory(prefix=".smoothnas-pbs-probe-", dir=path) as tmp:
        root = Path(tmp)
        source = root / "chunk.tmp"
        with source.open("wb") as handle:
            handle.write(b"PBS filesystem probe\n")
            handle.flush()
            os.fsync(handle.fileno())
        chunk = root / "chunk"
        source.rename(chunk)
        old = time.time_ns() - 3 * 24 * 3600 * 10**9
        os.utime(chunk, ns=(old, old))
        current = time.time_ns()
        os.utime(chunk, ns=(current, old))
        st = chunk.stat()
        if abs(st.st_atime_ns - current) > 10**9 or abs(st.st_mtime_ns - old) > 10**9:
            raise RuntimeError("Explicit chunk atime update did not preserve mtime")
        with chunk.open("rb") as first, chunk.open("rb") as second:
            fcntl.flock(first, fcntl.LOCK_EX | fcntl.LOCK_NB)
            try:
                fcntl.flock(second, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                pass
            else:
                raise RuntimeError("Conflicting datastore locks were both granted")
        fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    print("Filesystem probe passed: timestamp updates, locking, rename and fsync")


def client(args, *command):
    cmd = [args.client, *command]
    if args.namespace:
        cmd += ["--ns", args.namespace]
    subprocess.run(cmd, check=True)


def contents(root):
    result = {}
    for path in sorted(root.rglob("*")):
        name = str(path.relative_to(root))
        if path.is_symlink():
            result[name] = {"link": os.readlink(path)}
        elif path.is_file():
            result[name] = {
                "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                "mode": path.stat().st_mode & 0o777,
            }
    return result


def restore(args, record):
    # Keep the source/evidence between invocations; restored trees are disposable.
    with tempfile.TemporaryDirectory(prefix="restore-", dir=args.workdir) as tmp:
        target = Path(tmp) / "files"
        client(args, "restore", record["snapshot"], "data.pxar", str(target))
        if contents(target) != record["contents"]:
            raise RuntimeError("Restored hashes, symlinks or permissions differ")
    print("Restore passed: hashes, symlinks and permissions match")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--filesystem", type=Path, help="Probe a mounted datastore path locally")
    parser.add_argument("--workdir", type=Path, help="New evidence directory for network backup/restore")
    parser.add_argument("--restore-only", action="store_true", help="Restore an existing workdir after maintenance/reinstall")
    parser.add_argument("--client", default="proxmox-backup-client")
    parser.add_argument("--namespace", default="")
    args = parser.parse_args()
    if not args.filesystem and not args.workdir:
        parser.error("provide --filesystem and/or --workdir")
    if args.filesystem:
        filesystem_probe(args.filesystem)
    if not args.workdir:
        return
    if not os.environ.get("PBS_REPOSITORY"):
        parser.error("set PBS_REPOSITORY and PBS credentials in the environment")
    evidence = args.workdir / "validation.json"
    if args.restore_only:
        record = json.loads(evidence.read_text())
        if record["repository"] != os.environ["PBS_REPOSITORY"] or record["namespace"] != args.namespace:
            parser.error("repository and namespace must match the saved evidence")
        restore(args, record)
        return
    args.workdir.mkdir(mode=0o700, parents=True, exist_ok=False)
    source = args.workdir / "source"
    source.mkdir()
    (source / "payload").write_bytes(os.urandom(2 * 1024 * 1024))
    (source / "note").write_text("first version\n")
    (source / "note").chmod(0o640)
    (source / "link").symlink_to("note")
    group = "host/smoothnas-validation-" + uuid.uuid4().hex
    snapshots = []
    for version in (1, 2):
        stamp = int(time.time())
        (source / "note").write_text(f"version {version}\n")
        client(args, "backup", f"data.pxar:{source.resolve()}", "--backup-id", group.split("/")[1],
               "--backup-type", "host", "--backup-time", str(stamp), "--crypt-mode", "none")
        iso = datetime.datetime.fromtimestamp(stamp, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        snapshots.append(f"{group}/{iso}")
        if version == 1:
            time.sleep(1.1)
    record = {"snapshot": snapshots[-1], "contents": contents(source),
              "repository": os.environ["PBS_REPOSITORY"], "namespace": args.namespace}
    evidence.write_text(json.dumps(record, indent=2) + "\n")
    restore(args, record)
    # Retention applies ONLY to this new UUID group, never existing backups.
    client(args, "prune", group, "--keep-last", "1", "--dry-run", "false")
    restore(args, record)
    print(f"Backup/prune/restore passed for {group}")
    print("Run PBS verification and garbage collection, then rerun with --restore-only.")
    print("Repeat the restore after tier movement and uninstall/reinstall on the same tiers.")


if __name__ == "__main__":
    main()
