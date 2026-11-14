"""Record the contents and identity of candidate packages."""

import hashlib
import json
import re
import subprocess
import tarfile


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def commit():
    revision = command("git", "rev-parse", "HEAD")
    if subprocess.run(["git", "diff", "--quiet", "HEAD", "--"]).returncode:
        raise SystemExit("Candidate builds require tracked files matching HEAD")
    return revision


def source_archive(source, *paths):
    source.mkdir()
    with subprocess.Popen(["git", "archive", "--format=tar", "HEAD", *paths], stdout=subprocess.PIPE) as archive:
        with tarfile.open(fileobj=archive.stdout, mode="r|") as contents:
            contents.extractall(source, filter="data")
        if archive.wait():
            raise RuntimeError("Unable to extract the tracked source at HEAD")


def checksum(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def write_json(path, data):
    path.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")


def write_checksums(directory):
    names = sorted(file for file in directory.iterdir() if file.is_file() and file.name != "SHA256SUMS")
    content = "".join(f"{checksum(file)}  {file.name}\n" for file in names)
    (directory / "SHA256SUMS").write_text(content, encoding="ascii", newline="\n")


def check_version(output, version, revision):
    if output != f"rescuarr {version} ({revision})":
        raise SystemExit(f"Unexpected embedded build identity: {output!r}")


def sbom(path, binary, version, revision, alpine_packages=()):
    info = json.loads(command("go", "version", "-m", "-json", str(binary)))
    components = []
    for dependency in info.get("Deps") or ():
        installed = dependency.get("Replace") or dependency
        component = {
            "type": "library",
            "name": installed["Path"],
            "version": installed["Version"],
        }
        if not component["version"] or component["version"] == "(devel)":
            raise SystemExit(f"Unversioned dependency in candidate: {installed['Path']}")
        components.append(component)
    components.append({"type": "library", "name": "Go standard library", "version": info["GoVersion"]})
    for package in alpine_packages:
        components.append({"type": "library", "name": package["name"], "version": package["version"]})
    write_json(path, {
        "bomFormat": "CycloneDX",
        "specVersion": "1.6",
        "version": 1,
        "metadata": {
            "component": {
                "type": "application",
                "name": "Rescuarr",
                "version": version,
                "hashes": [{"alg": "SHA-256", "content": checksum(binary)}],
                "properties": [{"name": "rescuarr:source-revision", "value": revision}],
            },
        },
        "components": sorted(components, key=lambda component: (component["name"], component["version"])),
    })


def version_arg(value):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}", value):
        raise ValueError("Use letters, digits, dots, plus signs, underscores or hyphens in the version")
    return value
