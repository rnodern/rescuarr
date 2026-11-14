# Isolated upstream lab

Run `test/lab/run.sh /tmp/rescuarr-lab-evidence.json` from the repository on a Docker Engine with Compose v2 and Linux amd64 support. The argument must be a new output path. The runner pulls immutable images, starts its own project on an internal network with no host ports or host media mounts, exports allowlisted evidence, and tears the project down including its volumes. Each Arr has its own configuration and library. The downloader volume is shared deliberately. Use an unmodified checkout so the report's source SHA identifies the scripts tested.

The [Upstream lab workflow](../../.github/workflows/upstream-lab.yml) runs the same command on Ubuntu 24.04 and attaches the generated evidence. The files in [fixtures](fixtures) are hand-authored, source-shaped response examples for adapter tests. Their evidence class is `synthetic`. The CI report uses `observedRead` only for verified successful probes. No Arr or SABnzbd write is qualified by these files.

| Artifact | Pinned Linux build |
| --- | --- |
| Sonarr | [Upstream 4.0.20.3014](https://github.com/Sonarr/Sonarr/releases/tag/v4.0.20.3014), [LinuxServer ls328](https://github.com/linuxserver/sonarr/pkgs/container/sonarr) |
| Radarr | [Upstream 6.4.4.10685](https://github.com/Radarr/Radarr/releases/tag/v6.4.4.10685), [LinuxServer ls326](https://github.com/linuxserver/docker-radarr/pkgs/container/radarr) |
| SABnzbd | [Upstream 5.1.3](https://github.com/sabnzbd/sabnzbd/releases/tag/5.1.3), [LinuxServer Docker Hub](https://hub.docker.com/r/linuxserver/sabnzbd/tags) |

| Capability | Current evidence |
| --- | --- |
| Arr product/version, health, empty full queue | Read probes prepared for CI |
| SAB version, authenticated empty queue | Read probes prepared for CI |
| Tracked preview and reprocess, import file and provenance, Copy preservation and shared/partial paths | Tagged-source review. Awaiting disposable tests |
| Reject, blocklist, failed history, client effects, explicit search and lost response | Tagged-source review. Awaiting disposable tests |

Run the offline checks with `python3 -B -m unittest discover -s test/lab -p 'test_*.py'`. See [integration contracts](../../docs/INTEGRATIONS_DESIGN.md) for the broader release criteria.
