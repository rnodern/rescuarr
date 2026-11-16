# Install a Rescuarr candidate

The Go candidate uses a local SQLite database and opens its web console at `http://127.0.0.1:8787`. Run one process per data directory and keep the data on a local filesystem. Rescuarr uses the Sonarr, Radarr and optional SABnzbd HTTP APIs, so its runtime needs no media or Docker socket mounts.

The **Native candidate** and **Container candidate** Actions runs supply candidate artifacts. Select a successful run at the exact source SHA you want and confirm `source_sha` in its `build.json`. Pull request builds identify the head SHA. The runs on main verify the merged source. Each package contains `SHA256SUMS`, a CycloneDX inventory and build metadata. CI candidates are unsigned. macOS CI candidates also lack notarisation. Follow your platform's security policy when evaluating these builds.

| Package | Target |
| --- | --- |
| `linux-amd64`, `linux-arm64` tar.gz | Linux x86-64 and ARM64 |
| `darwin-amd64`, `darwin-arm64` zip | macOS Intel and Apple silicon |
| `windows-amd64`, `windows-arm64` zip | Windows x64 and ARM64 |
| `linux-amd64`, `linux-arm64` OCI tar | Linux x86-64 and ARM64 Docker hosts |

## Verify and start a native package

Run the checksum command from the downloaded package directory before extracting the archive.

| Platform | Verify all files |
| --- | --- |
| Linux | `sha256sum -c SHA256SUMS` |
| macOS | `shasum -a 256 -c SHA256SUMS` |
| Windows PowerShell | Use the script below. |

```powershell
Get-Content SHA256SUMS | ForEach-Object {
    $expected, $name = $_ -split '  ', 2
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $name).Hash
    if ($expected -ne $actual) { throw "Checksum failed for $name" }
}
```

On Linux, substitute the archive filename for `ARCHIVE.tar.gz` and run these commands. On macOS use `ditto -x -k ARCHIVE.zip rescuarr-bin` in place of the tar command. Ctrl+C stops the foreground process.

```sh
mkdir -p rescuarr-bin
tar -xzf ARCHIVE.tar.gz -C rescuarr-bin
./rescuarr-bin/rescuarr version
./rescuarr-bin/rescuarr doctor
./rescuarr-bin/rescuarr serve
```

On Windows, use PowerShell under your own account.

```powershell
Expand-Archive -LiteralPath 'ARCHIVE.zip' -DestinationPath '.\rescuarr-bin'
.\rescuarr-bin\rescuarr.exe version
.\rescuarr-bin\rescuarr.exe doctor
.\rescuarr-bin\rescuarr.exe serve
```

`doctor` checks the local configuration. Run it before starting Rescuarr. Open the console while it is serving. Use the one-time token in the `setup-token` file in the data directory to initialise the owner account. Keep that file private while it exists.

| Platform | Default local data directory |
| --- | --- |
| Linux | `$XDG_STATE_HOME/rescuarr` when set, otherwise `$HOME/.local/state/rescuarr` |
| macOS | `$HOME/Library/Application Support/Rescuarr` |
| Windows | `%LOCALAPPDATA%\Rescuarr` |
| Container | `/data` on a persistent volume |

Use `--data-dir` or `RESCUARR_DATA_DIR` to override the location. On Unix use a private directory owned by the runtime user, with permissions `0700`. On Windows use an account-owned local directory with access limited to that account and appropriate administrators. If overriding the default, check inheritance with `icacls`. `--listen` or `RESCUARR_LISTEN` sets the native listening address.

### Optional startup at login or boot

| Platform | Lifecycle |
| --- | --- |
| Linux systemd | Create an unprivileged system user and group named `rescuarr`. Copy the executable to `/usr/local/bin/rescuarr` as root, mode `0755`, and copy [rescuarr.service](../deploy/systemd/rescuarr.service) to `/etc/systemd/system/`. Run `sudo systemctl daemon-reload` and `sudo systemctl enable --now rescuarr`. Stop via `sudo systemctl stop rescuarr`, check logs via `sudo journalctl -u rescuarr`. The unit uses `/var/lib/rescuarr` and optionally reads `/etc/rescuarr/rescuarr.env`. |
| macOS launchd | In the [launch agent](../deploy/launchd/com.github.rnodern.rescuarr.plist), substitute the absolute executable path. Save it in `~/Library/LaunchAgents` under the same filename. Run `launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.github.rnodern.rescuarr.plist"`. To stop it, run `launchctl bootout "gui/$(id -u)/com.github.rnodern.rescuarr"`. |
| Windows | Run the portable executable interactively. Windows Service Control Manager integration awaits platform qualification. |

## Linux container

Use [the Compose example](../deploy/container/compose.yaml) from the candidate source checkout. From the repo root, verify the OCI archive with `sha256sum -c SHA256SUMS` in the download directory. Check its manifest digest against `build.json` with `skopeo inspect --raw oci-archive:/absolute/path/to/ARCHIVE.oci.tar | sha256sum`. Import that verified archive into the local Docker engine and use the matching architecture in the image name.

```sh
skopeo --insecure-policy copy oci-archive:/absolute/path/to/ARCHIVE.oci.tar docker-daemon:rescuarr:candidate-amd64
export RESCUARR_IMAGE=rescuarr:candidate-amd64
docker compose -f deploy/container/compose.yaml up -d --no-build --pull never
docker compose -f deploy/container/compose.yaml exec -T rescuarr rescuarr version
```

The `--insecure-policy` flag selects signature-policy bypass for this local unsigned archive import after checksum verification. The workflow saves one architecture per archive. `oci_manifest_digest` is that archive's platform manifest digest. `oci_config_digest` is the expected Docker image ID after importing.

To build locally from the candidate checkout, run `docker compose -f deploy/container/compose.yaml build` then `up -d --no-build --pull never` with the same Compose file. Keep the same Compose project name across upgrades to reuse the existing named volume. Start with the `data` named volume, which uses the image's `0700` directory owned by UID and GID `10001`. For a bind mount, prepare a private local directory owned by the daemon-mapped runtime UID and GID before starting. `docker compose -f deploy/container/compose.yaml down` stops the app and preserves its named volume. The `--volumes`/ `-v` option deletes that data.

The example publishes only on `127.0.0.1`. The default browser address uses `RESCUARR_HOST_PORT`. Set `RESCUARR_PUBLIC_URL` to the exact operator-facing URL when using a reverse proxy. When an integration supports file-based credentials, mount the individual file read-only, accessible to the runtime user, then configure its reference in Rescuarr. Avoid storing credentials in image layers or Compose YAML.

## Manual Review

Open Overview and choose **Switch to Manual**. Manual Review shows one download until you approve or decline. A decline is recorded in Activity.

For a completed season pack with no eligible files, approval removes it from Sonarr, blocklists the exact release and searches only the reviewed episodes that are still missing. It retains the SABnzbd job.

For a completed Usenet download that Sonarr matched by series ID, Manual Review can approve one exact video file for one monitored episode that still needs its first library file. When the audio decision is **Review**, confirm the video and audio before approving.

The Copy profile with hardlinks and SABnzbd removal disabled verifies that the source is still present. Native Windows Sonarr also supports the profile with **Use Hardlinks instead of Copy**, **Remove Completed** and **Remove Failed** enabled. Manual Review shows that Sonarr can remove the source folder and the job from active SABnzbd history in this profile. Activity verifies the approved episode and its library file. Sonarr manages the download cleanup.

Each approval route requires a locally verified connection. Check its queue in Rescuarr, stop the app, and run the diagnostic for the route you want to enable. Supply the same data directory that Rescuarr normally uses.

| Route | Diagnostic | Environment variable prefix |
| --- | --- | --- |
| Empty season pack | `rescuarr check-empty-pack --data-dir /path/to/data --sonarr-connection ID` | `RESCUARR_MANUAL_EMPTY_PACK` |
| Sonarr Copy import | `rescuarr check-manual-import --data-dir /path/to/data --sonarr-connection ID` | `RESCUARR_MANUAL_IMPORT` |

For a verified route, add `_CONNECTION` with the connection ID, `_PROFILE` with the reported SHA-256, and `_SAB_VERSION` with the version verified on the SABnzbd host to the route's prefix. The tested versions are Sonarr 5.0.0.2941 develop and SABnzbd 5.1.3. The diagnostic checks route-specific Sonarr settings, including path mappings and download cleanup. Rescuarr checks the profile again before dispatch. You can leave Manual mode from Overview. An in-flight action retains its journal record.

To show audio track tags before approval, install FFmpeg with `ffprobe` on the Rescuarr host. Set `RESCUARR_MANUAL_IMPORT_AUDIO_SONARR_ROOT` to the completed downloads directory as Sonarr sees it, and `RESCUARR_MANUAL_IMPORT_AUDIO_LOCAL_ROOT` to the absolute path to that same directory on the Rescuarr host. Use a read-only mount if the directory is on another machine. The probe applies to the pinned Sonarr connection and reads only the selected source file. When the file or its tags are unavailable, Manual Review applies the existing policy for unverified audio. Track tags are metadata, so a file can still be mistagged.

## Automatic import dry run

Open **Import dry run** and choose **Run dry run** to check a page of held downloads. It reads the Sonarr queue, exact episode and candidate file, tagged audio, library space and the pinned import profile. A candidate passed this read-only assessment. Sonarr still has to approve reprocessing the exact episode before it can be imported. Unverified audio is shown as needing review.

If a series has different episode orders across sources, open one of its downloads in the Recovery Queue and choose **Require matching episode title** under **Episode order**. The setting applies to this series on this Sonarr connection. The dry run holds downloads unless their folder or filename confirms Sonarr’s episode title. Manual Review can still use a match on series and episode number for an owner to verify.

## Automatic Sonarr imports

Set up the verified Sonarr Copy profile and source audio reader described under Manual Review. On Windows, Rescuarr can run alongside Sonarr and SABnzbd with both audio roots set to the same local completed-downloads directory. Install FFmpeg with `ffprobe` on the Rescuarr process’s PATH. Use the actual completed-downloads path from this Sonarr instance.

When moving an installation, stop Rescuarr and transfer its private data directory, including the database and secrets, to an account-owned directory on the Windows host. This carries over Manual Review decisions and per-series episode-order settings. Save the active data path as `$RescuarrData` in PowerShell, then run `check-manual-import` with that path while the service is stopped. Use its reported connection ID and profile SHA-256 and the SABnzbd version verified on this host.

```powershell
$env:RESCUARR_MANUAL_IMPORT_CONNECTION = "<reported connection ID>"
$env:RESCUARR_MANUAL_IMPORT_PROFILE = "<reported profile SHA-256>"
$env:RESCUARR_MANUAL_IMPORT_SAB_VERSION = "5.1.3"
$env:RESCUARR_MANUAL_IMPORT_AUDIO_SONARR_ROOT = "C:\path\to\completed-downloads"
$env:RESCUARR_MANUAL_IMPORT_AUDIO_LOCAL_ROOT = $env:RESCUARR_MANUAL_IMPORT_AUDIO_SONARR_ROOT
.\rescuarr.exe serve --data-dir $RescuarrData --listen 127.0.0.1:18788
```

Run **Import dry run** to check candidate eligibility. From Overview, choose **Enable automatic Sonarr imports**. Rescuarr checks and imports one verified single-episode Usenet download at a time on the pinned Sonarr connection. It uses Sonarr’s configured cleanup behaviour and records the import result in Activity. A download with incomplete audio evidence, an unresolved identity, or insufficient space stays held.

Choose **Switch to Manual** on Overview to review held downloads or **Pause all recovery actions** to stop new sends. An unresolved send occupies the import slot while Rescuarr reads back Sonarr’s result, including after restart. If confirmation stays unavailable, check Sonarr and close the unresolved review from Manual Review.

## Reverse proxy

For LAN access, put an HTTPS proxy in front of Rescuarr and preserve the client-facing host and path. Set `RESCUARR_PUBLIC_URL` to the complete external URL, such as `https://rescuarr.example.net`. If using `https://example.net/rescuarr`, also set `RESCUARR_BASE_PATH=/rescuarr` and forward the path without stripping the prefix. Set `RESCUARR_TRUSTED_PROXIES` to the proxy ingress addresses as comma-separated CIDRs. Configure the proxy to replace incoming forwarded headers. For example, a proxy on the same host may use `127.0.0.1/32`. Point local liveness and readiness probes to the fixed `/healthz` and `/readyz` paths.

## Upgrade, backup and restore

1. Record the current version and SHA. Stop the current app and back up the local database with the current executable and its runtime account. Keep the database snapshot in a private location and handle any externally managed secret files according to their own backup procedures.
2. Install and verify the new executable or import its OCI archive. Start it with the existing data directory. Check health, readiness and the console.
3. For rollback, stop the new build. Use the prior executable to restore its backup into a new, empty directory. Start the prior build with that restored directory. Check its settings and audit and review pending work before resuming recovery.

For a native install, use explicit private paths and the service account where applicable.

```sh
rescuarr backup --data-dir /path/to/live-data --output /private/path/rescuarr-backup.sqlite3
rescuarr restore --data-dir /path/to/new-data --input /private/path/rescuarr-backup.sqlite3
```

For Docker, stop the service with `docker compose -f deploy/container/compose.yaml stop rescuarr`. Mount a private host backup directory writable by UID/GID `10001` and run the maintenance command against the stopped service's volume.

```sh
docker compose -f deploy/container/compose.yaml run --rm --no-deps \
  --volume /private/backup-dir:/backup rescuarr backup --output /backup/rescuarr-backup.sqlite3
```

For a restore, prepare a fresh Compose project name and the matching prior image. This creates a separate empty data volume. The backup directory needs to be readable by the runtime account.

```sh
docker compose -f deploy/container/compose.yaml -p rescuarr-restore run --rm --no-deps \
  --volume /private/backup-dir:/backup:ro rescuarr restore --input /backup/rescuarr-backup.sqlite3
docker compose -f deploy/container/compose.yaml -p rescuarr-restore up -d --no-build --pull never
```

## Build and signing evidence

Native candidates can be built locally from the exact clean source SHA with Go from `go.mod` and Python 3.13. Example for a Linux ARM64 machine:

```sh
GOTOOLCHAIN=local python3 scripts/packaging/native.py --os linux --arch arm64 --output dist/native
```

Native packaging takes an optional `--sign-hook PATH`. The hook must be a local executable that receives the built binary path, applies the platform signature, verifies it and returns a failure if signing or verification fails. Use `--require-signature` to fail when a hook is missing. For macOS, use `--notarise-hook PATH` with the signing hook. The notarisation hook receives the final zip path, submits it with authorised Apple Developer ID credentials and returns success only after confirming Apple accepted it. Use `--require-notarisation` to fail if the hook is missing. Candidate metadata records hook completion. Preserve or regenerate checksums and evidence after any archive changes. CI requires no signing material and records the actual unsigned status. Verification of the full platform signing and notarisation path requires the relevant identities and runner access.
