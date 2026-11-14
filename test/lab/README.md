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

Pass one of `sonarr/import`, `radarr/import`, `sonarr/reject-preserve`, `radarr/reject-preserve`, `sonarr/reject-remove`, `radarr/reject-remove`, `sonarr/reject-lost-response`, `radarr/reject-lost-response`, `sonarr/replace-terminal` or `radarr/replace-terminal` as the second argument. The effect runner also requires OpenSSL and FFmpeg. It generates disposable certificates and a video from filters, serves controlled metadata, search results and articles on the closed network, and exports scoped evidence. GitHub pull requests run the read probes and the isolated Sonarr 5 scenarios as separate jobs. Use the workflow dispatch input to run one effect scenario on an exact checked-out revision.

The import scenario records whether the source was preserved as a separate result. The rejection scenarios exercise the Arr queue branch on a paused synthetic download. They do not establish terminal downloader failure, qualify automatic removal, or qualify the production replacement flow. A source-retaining trial can leave final import pending. Review each effect independently before drawing a capability conclusion.

### Sonarr 5 series-ID import

Run `test/lab/run.sh /tmp/rescuarr-sonarr-v5-id.json sonarr/v5-id-import` for the isolated [Sonarr 5.0.0.2941 prerelease](https://github.com/Sonarr/Sonarr/releases/tag/v5.0.0.2941) image with SABnzbd. The other scenarios keep the Sonarr 4 pin.

An episode search grabs a synthetic release with an abbreviated series title and the exact TVDB ID. The scenario requires Sonarr's own grab history to report an ID match and its completed queue row to carry the series-ID import warning. Completed-download handling stays on so Sonarr publishes the completed row. Completed-file removal stays off. It checks the one staged file, submits the exact series, season and episode to native reprocessing, and checks the native rejections before sending one Copy import command.

The evidence records the imported file ID, contents, matching history and disappearance of the exact queue row. It independently compares the original folder inventory and bytes, downloader state and the neighbouring episode before and after. The scenario exercises the upstream path in the lab. Runtime approval and effects in the Rescuarr application require their own evidence.

### Sonarr 5 completed season pack with no eligible files

Run `test/lab/run.sh /tmp/rescuarr-sonarr-v5-empty-pack.json sonarr/v5-empty-pack`, or dispatch the workflow with `-f scenario=sonarr/v5-empty-pack` on a ref containing this scenario. The PR job also runs on the exact PR head and uploads `sonarr-v5-empty-pack-evidence`.

This scenario uses the pinned Sonarr 5 and SABnzbd Linux images. A native season search grabs a synthetic pack. The runner waits for SABnzbd to report the exact job completed, moves its single synthetic video out of the download folder and stages it under episode 2 in the disposable library. A native library rescan must link that file to episode 2 only. The runner then enables completed-download handling and requires Sonarr's no-eligible-files warning on every alias of the original pack.

The runner applies one native queue removal with blocklisting, client retention and immediate redownload suppressed. It checks failed history for both aliases, one matching blocklist row, queue absence through refresh, unchanged SABnzbd history, unchanged empty folder and no implicit search. It then searches only for the still-missing episode 1 and verifies a distinct job imported into its file link while the existing episode 2 file stayed unchanged. The scenario contributes Linux upstream evidence when executed successfully. A Windows path, deployment wiring, all-covered behavior and active alternative queue jobs require separate verification.

### Sonarr 5 empty-pack Go adapter

Run `test/lab/run.sh /tmp/rescuarr-sonarr-v5-adapter-pack.json sonarr/v5-adapter-pack` with the Go version pinned in `go.mod`. This scenario cross-compiles the exact checked-out Go adapter as a Linux amd64 probe and mounts it read-only into the isolated runner. The probe's transport accepts only the fixed lab Sonarr origin. It uses the lab credential via stdin and reports allowlisted outcomes.

The Go probe performs preflight, sends the one scoped removal, checks its readback and submits the episode-only search. Python independently confirms the queue, blocklist, SAB history, source preservation, command completion and resulting library file. The report records the checkout SHA and pinned images. Fixture qualification is local to this lab process. A passing run provides Go adapter evidence for the pinned Linux environment. See the empty-pack scenario above for remaining Windows and product-runtime coverage.

### Native Windows empty-pack lab

The [Windows upstream workflow](../../.github/workflows/windows-upstream-lab.yml) runs the same empty-pack assertions with the native Windows x64 portable builds of Sonarr 5.0.0.2941 develop and SABnzbd 5.1.3. It launches both applications with fresh data directories on a disposable GitHub-hosted Windows Server 2025 runner. Sonarr and SABnzbd use separate loopback addresses and dedicated ports. The synthetic indexer, media and NNTP server stay on that runner. The workflow briefly directs Sonarr's metadata hostname to the synthetic fixture inside the runner and removes its generated trust certificate afterwards.

The checkout-specific `sonarr-windows-v5-adapter-pack-evidence` artefact records the native queue and library results. A passing run contributes evidence for the native Windows adapter on a local drive. Windows service accounts and network library paths require additional evidence.

The same runner accepts `--rescuarr` with the executable extracted from a native candidate archive for the checked-out SHA. It uses authenticated Manual Review forms in two fresh Rescuarr installations against the isolated Arr test queue. It exercises Decline and confirms the recorded decision has no upstream effect, checks that the pending review survives reload and restart, then approves the single-episode replacement. The artefact records the packaged version and looks up the native search command, queue and blocklist outcomes, preserved SABnzbd job, and verified import. This scenario covers Manual mode on the disposable Windows setup.

### Terminal replacement

The terminal scenarios request an original release whose fixture articles return NNTP `430`. They join the original Arr grab to the exact SABnzbd `Failed` history entry and record queue absence and the media inventory of the reported output paths. Arr's download monitoring handles the failure, with automatic redownload and failed-job removal disabled. The report includes the new Arr failure history entry, matching blocklist entry and exact job absence from the Arr queue.

After observing those effects, the runner publishes the replacement to the synthetic indexer and explicitly searches for the same episode or movie. It records the distinct SABnzbd job, completed download, linked library file, matching import history and the file's SHA-256. It checks that the failed original's disposition and the unrelated episode or movie's file state are unchanged. The terminal report covers Arr's native monitoring and an explicit search in the disposable lab. Queue `DELETE` and client removal are covered by the separate queue scenarios.
