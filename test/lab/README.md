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

## Optional effect scenarios

Pass one of `sonarr/import`, `radarr/import`, `sonarr/reject-preserve`, `radarr/reject-preserve`, `sonarr/reject-remove`, `radarr/reject-remove`, `sonarr/reject-lost-response`, `radarr/reject-lost-response`, `sonarr/replace-terminal` or `radarr/replace-terminal` as the second argument. The effect runner also requires OpenSSL and FFmpeg. It generates disposable certificates and a video from filters, serves controlled metadata, search results and articles on the closed network, and exports scoped evidence. GitHub pull requests run the read probes plus the isolated Sonarr 5 ID-only scenario as a separate job. Use the workflow dispatch input to run one effect scenario on an exact checked-out revision.

The import scenario records whether the source was preserved as a separate result. The rejection scenarios exercise the Arr queue branch on a paused synthetic download. They do not establish terminal downloader failure, qualify automatic removal, or qualify the production replacement flow. A source-retaining trial can leave final import pending. Review each effect independently before drawing a capability conclusion.

### Sonarr 5 series-ID import

Run `test/lab/run.sh /tmp/rescuarr-sonarr-v5-id.json sonarr/v5-id-import` for the isolated [Sonarr 5.0.0.2941 prerelease](https://github.com/Sonarr/Sonarr/releases/tag/v5.0.0.2941) image. The other scenarios keep the Sonarr 4 pin.

An episode search grabs a synthetic release with an abbreviated series title and the exact TVDB ID. The scenario requires Sonarr's own grab history to report an ID match and its completed queue row to carry the series-ID import warning. It checks the one staged file, submits the exact series, season and episode to native reprocessing, and checks the native rejections before sending one Copy import command.

The evidence records the imported file ID, contents, matching history and disappearance of the exact queue row. It independently compares the original folder inventory and bytes, downloader state and the neighbouring episode before and after. The scenario exercises the upstream path in the lab. Runtime approval and effects in the Rescuarr application require their own evidence.

### Terminal replacement

The terminal scenarios request an original release whose fixture articles return NNTP `430`. They join the original Arr grab to the exact SABnzbd `Failed` history entry and record queue absence and the media inventory of the reported output paths. Arr's download monitoring handles the failure, with automatic redownload and failed-job removal disabled. The report includes the new Arr failure history entry, matching blocklist entry and exact job absence from the Arr queue.

After observing those effects, the runner publishes the replacement to the synthetic indexer and explicitly searches for the same episode or movie. It records the distinct SABnzbd job, completed download, linked library file, matching import history and the file's SHA-256. It checks that the failed original's disposition and the unrelated episode or movie's file state are unchanged. The terminal report covers Arr's native monitoring and an explicit search in the disposable lab. Queue `DELETE` and client removal are covered by the separate queue scenarios.
