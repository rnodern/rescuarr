"""Create a native candidate archive and verify the binary extracted from it."""

import argparse
from contextlib import contextmanager
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import socket
import subprocess
import tempfile

from artifact import check_version, checksum, command, commit, sbom, source_archive, version_arg, write_checksums, write_json
from smoke import check_owner_workflow, check_setup_page, check_setup_token, installation_state, wait_for_http


def smoke_log_tail(log, scratch):
    limit = 2048
    log.seek(max(0, log.seek(0, os.SEEK_END) - limit))
    tail = log.read(limit).decode("utf-8", errors="replace").strip()
    prefix = re.escape(str(scratch))
    tail = re.sub(prefix, "<smoke-dir>", tail, flags=re.IGNORECASE if os.name == "nt" else 0)
    tail = re.sub(r"(?<![A-Za-z0-9_-])[A-Za-z0-9_-]{43}(?![A-Za-z0-9_-])", "<token>", tail)
    return tail or "<empty>"


@contextmanager
def server(binary, data, phase):
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    args = [str(binary), "serve", "--data-dir", str(data), "--listen", f"127.0.0.1:{port}"]
    environment = {key: value for key, value in os.environ.items() if not key.startswith("RESCUARR_")}
    # Only output from this process can be included in smoke failure diagnostics.
    with (data.parent / "smoke.log").open("w+b") as log:
        process = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, env=environment)
        status = None
        try:
            try:
                base = f"http://127.0.0.1:{port}"
                wait_for_http(base, lambda: process.poll() is None)
                check_setup_page(base)
                yield base
            finally:
                status = process.poll()
                if status is None:
                    process.send_signal(signal.SIGTERM)
                    try:
                        process.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
            if status is not None or (os.name != "nt" and process.returncode != 0):
                raise RuntimeError("Packaged process stopped unexpectedly")
        except Exception as error:
            exit_status = f"exit status {status}" if status is not None else f"shutdown status {process.returncode}"
            tail = smoke_log_tail(log, data.parent)
            raise RuntimeError(f"Packaged process failed during {phase} ({exit_status}). Log tail {tail!r}") from error


def smoke(binary, scratch, version, revision):
    check_version(command(str(binary), "version"), version, revision)
    data = scratch / "original"
    database = data / "rescuarr.sqlite3"
    with server(binary, data, "first startup"):
        check_setup_token(data)
        install_id, _, _ = installation_state(database)
    with server(binary, data, "restart"):
        if installation_state(database)[0] != install_id:
            raise RuntimeError("Installation ID changed after restart")

    backup = scratch / "backup.sqlite3"
    restored = scratch / "restored"
    command(str(binary), "backup", "--data-dir", str(data), "--output", str(backup))
    command(str(binary), "restore", "--data-dir", str(restored), "--input", str(backup))
    if installation_state(restored / database.name) != (install_id, "observe", 1):
        raise RuntimeError("Restore failed to preserve the installation ID and gate recovery")
    with server(binary, restored, "startup after restore") as base:
        if installation_state(restored / database.name)[0] != install_id:
            raise RuntimeError("Restored installation failed to start with its original identity")
        check_owner_workflow(base, restored)


def build(args):
    revision = commit()
    goos, goarch, host_arch = command("go", "env", "GOHOSTOS", "GOHOSTARCH", "GOARCH").splitlines()
    machine = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}
    if (goos, goarch, host_arch) != (args.os, args.arch, args.arch) or machine.get(platform.machine().lower()) != args.arch:
        raise SystemExit(f"Native target mismatch: requested {args.os}/{args.arch}, Go host {goos}/{goarch}, "
                         f"GOARCH={host_arch}, machine={platform.machine()}")
    if command("go", "env", "GOOS") != args.os:
        raise SystemExit("GOOS does not match the host operating system")

    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    with tempfile.TemporaryDirectory(prefix="rescuarr-native-") as temporary:
        scratch = Path(temporary)
        source = scratch / "source"
        source_archive(source, "go.mod", "go.sum", "cmd", "internal", "docs/INSTALL.md", "deploy")
        package = scratch / "package"
        package.mkdir()
        binary = package / ("rescuarr.exe" if args.os == "windows" else "rescuarr")
        print(f"Building native {args.os}/{args.arch} executable", flush=True)
        subprocess.run(
            ["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
             "-ldflags", f"-s -w -X main.version={args.version} -X main.commit={revision}",
             "-o", str(binary), "./cmd/rescuarr"],
            cwd=source, env=os.environ | {"CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOWORK": "off"},
            check=True,
        )
        if args.sign_hook:
            subprocess.run([str(args.sign_hook.resolve()), str(binary)], check=True)
        (package / "docs").mkdir()
        shutil.copyfile(source / "docs/INSTALL.md", package / "docs/INSTALL.md")
        shutil.copytree(source / "deploy", package / "deploy")
        name = f"rescuarr-{args.version}-{args.os}-{args.arch}"
        archive_format = "gztar" if args.os == "linux" else "zip"
        archive = Path(shutil.make_archive(str(output / name), archive_format, root_dir=package))
        if args.notarise_hook:
            subprocess.run([str(args.notarise_hook.resolve()), str(archive)], check=True)
        extracted = scratch / "extracted"
        if args.os == "darwin":
            subprocess.run(["ditto", "-x", "-k", str(archive), str(extracted)], check=True)
        else:
            shutil.unpack_archive(archive, extracted, filter="data" if archive_format == "gztar" else None)
        installed = extracted / binary.name
        if checksum(installed) != checksum(binary):
            raise RuntimeError("Archived executable differs from the build output")
        print(f"Starting smoke tests on the extracted {args.os}/{args.arch} executable", flush=True)
        smoke(installed, scratch, args.version, revision)
        sbom(output / "sbom.cdx.json", installed, args.version, revision)
        write_json(output / "build.json", {
            "version": args.version, "source_sha": revision, "platform": f"{args.os}/{args.arch}",
            "go": command("go", "version"), "archive": archive.name,
            "archive_sha256": checksum(archive), "binary_sha256": checksum(installed),
            "runtime": {"runner": os.environ.get("RUNNER_NAME", platform.platform()),
                        "checks": ["version", "health", "readiness", "setup page", "restart", "backup", "restore", "owner setup and sign-in", "case and connection pages", "connection key save", "session logout"]},
            "signing": "external hook completed" if args.sign_hook else "unsigned",
            "notarisation": (
                "external hook completed" if args.notarise_hook else
                "not performed" if args.os == "darwin" else "not applicable"
            ),
        })
    write_checksums(output)
    print(f"Verified native {args.os}/{args.arch} archive at {revision}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--os", required=True, choices=["linux", "darwin", "windows"])
    parser.add_argument("--arch", required=True, choices=["arm64", "amd64"])
    parser.add_argument("--version", type=version_arg, default="candidate")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--sign-hook", type=Path, help="local executable that signs and verifies the binary")
    parser.add_argument("--require-signature", action="store_true")
    parser.add_argument("--notarise-hook", type=Path, help="local executable that submits and verifies a macOS zip")
    parser.add_argument("--require-notarisation", action="store_true")
    args = parser.parse_args()
    if args.require_signature and not args.sign_hook:
        parser.error("--require-signature needs a local signing hook")
    if args.require_notarisation and not args.notarise_hook:
        parser.error("--require-notarisation needs a local notarisation hook")
    if args.notarise_hook and (args.os != "darwin" or not args.sign_hook):
        parser.error("--notarise-hook requires a macOS target and --sign-hook")
    build(args)
