"""Verify the OCI candidate in Compose and inventory the loaded image."""

import json
from pathlib import Path
import os
import subprocess
import sys
import tarfile

from artifact import check_version, checksum, command, sbom, write_checksums, write_json
from smoke import check_setup_page, installation_state, wait_for_http


def image_manifest(archive, architecture):
    with tarfile.open(archive) as layout:
        index = json.load(layout.extractfile("index.json"))
        descriptors = index["manifests"]
        if len(descriptors) != 1:
            raise RuntimeError("OCI export must contain one native manifest")
        descriptor = descriptors[0]
        if descriptor["mediaType"] != "application/vnd.oci.image.manifest.v1+json":
            raise RuntimeError("Unexpected OCI manifest media type")
        descriptor_path = "blobs/" + descriptor["digest"].replace(":", "/")
        manifest = json.load(layout.extractfile(descriptor_path))
        config_path = "blobs/" + manifest["config"]["digest"].replace(":", "/")
        config = json.load(layout.extractfile(config_path))
    if (config["os"], config["architecture"]) != ("linux", architecture):
        raise RuntimeError("Unexpected platform in OCI config")
    return descriptor["digest"], manifest["config"]["digest"]


def verify(architecture, version, revision, archive, project, scratch):
    compose = ["docker", "compose", "--env-file", "/dev/null", "--file", "compose.yaml", "--project-name", project]
    run = [*compose, "exec", "-T", "rescuarr"]
    manifest_digest, config_digest = image_manifest(archive, architecture)
    container = command(*compose, "ps", "-q", "rescuarr")
    details = json.loads(command("docker", "inspect", container))[0]
    host = details["HostConfig"]
    ports = host["PortBindings"]["8787/tcp"]
    port = os.environ["RESCUARR_HOST_PORT"]
    if details["Image"] != config_digest:
        raise RuntimeError("Running container image is not the exported OCI image")
    labels = details["Config"]["Labels"]
    if labels.get("org.opencontainers.image.revision") != revision or labels.get("org.opencontainers.image.version") != version:
        raise RuntimeError("Container labels don't match the candidate source and version")
    if (command(*run, "id", "-u"), command(*run, "id", "-g")) != ("10001", "10001"):
        raise RuntimeError("Container user/group should be 10001/10001")
    if ports != [{"HostIp": "127.0.0.1", "HostPort": port}] or not host["ReadonlyRootfs"]:
        raise RuntimeError("Compose should restrict host publishing and the root filesystem")
    if "ALL" not in host["CapDrop"] or "no-new-privileges:true" not in host["SecurityOpt"]:
        raise RuntimeError("Compose should drop capabilities and deny privilege escalation")
    if not any(mount["Destination"] == "/data" and mount["Type"] == "volume" and mount["RW"]
               for mount in details["Mounts"]):
        raise RuntimeError("Compose must mount a persistent writable local volume at /data")

    check_version(command(*run, "rescuarr", "version"), version, revision)
    base_url = f"http://127.0.0.1:{port}"
    wait_for_http(base_url, lambda: True)
    check_setup_page(base_url)
    subprocess.run([*run, "test", "-s", "/data/setup-token"], check=True)
    if command(*run, "stat", "-c", "%a", "/data/setup-token") != "600":
        raise RuntimeError("Setup token is not private inside the container")

    def snapshot(name):
        # Stop the server to release the data lock. Use the same image for maintenance.
        command(*compose, "stop", "rescuarr")
        backup = f"/data/{name}.sqlite3"
        command(*compose, "run", "--rm", "--no-deps", "-T", "rescuarr", "backup", "--output", backup)
        directory = command(*compose, "ps", "-aq", "rescuarr")
        destination = scratch / f"{name}.sqlite3"
        command("docker", "cp", f"{directory}:{backup}", str(destination))
        return installation_state(destination)

    install_id, _, _ = snapshot("original")
    command(*compose, "up", "--detach", "--no-build", "--pull", "never", "--force-recreate")
    wait_for_http(base_url, lambda: True)
    check_setup_page(base_url)
    if snapshot("restarted")[0] != install_id:
        raise RuntimeError("Installation ID changed after replacing the container")
    command(*compose, "run", "--rm", "--no-deps", "-T", "rescuarr", "restore", "--input", "/data/original.sqlite3")
    command(*compose, "up", "--detach", "--no-build", "--pull", "never")
    wait_for_http(base_url, lambda: True)
    check_setup_page(base_url)
    if snapshot("restored") != (install_id, "observe", 1):
        raise RuntimeError("Container restore failed to preserve the installation ID and gate recovery")

    container = command(*compose, "ps", "-aq", "rescuarr")
    binary = scratch / "rescuarr"
    command("docker", "cp", f"{container}:/usr/local/bin/rescuarr", str(binary))
    packages = json.loads(command(*compose, "run", "--rm", "--no-deps", "-T", "--entrypoint", "/sbin/apk",
                                  "rescuarr", "query", "--installed", "--fields", "name,version", "--format", "json", "*"))
    if not packages:
        raise RuntimeError("No installed Alpine packages in the container SBOM")

    output = archive.parent
    sbom(output / "sbom.cdx.json", binary, version, revision, packages)
    write_json(output / "build.json", {
        "version": version, "source_sha": revision, "platform": f"linux/{architecture}",
        "archive": archive.name, "archive_sha256": checksum(archive),
        "oci_manifest_digest": manifest_digest, "oci_config_digest": config_digest,
        "runtime": {"runner": os.environ.get("RUNNER_NAME", os.uname().machine),
                    "checks": ["version", "health", "readiness", "setup page", "non-root", "confinement",
                               "backup", "persistence after recreate", "restore"]},
        "signing": "unsigned",
    })
    write_checksums(output)
    print(f"Verified native OCI linux/{architecture} at {revision}: {manifest_digest}")


if __name__ == "__main__":
    architecture, version, revision, archive, project, scratch = sys.argv[1:]
    verify(architecture, version, revision, Path(archive), project, Path(scratch))
