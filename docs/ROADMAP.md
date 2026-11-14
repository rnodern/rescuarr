# Rescuarr delivery and verification plan

The public repository contains the original Python connectivity prototype. The production plan uses a single Go process and a local SQLite database. Build only after the design review. Use the [system design](ARCHITECTURE.md), [recovery specification](RECOVERY_DESIGN.md), [service integration contracts](INTEGRATIONS_DESIGN.md), and [web interface design](UI_DESIGN.md) as the requirements for the work below.

## 1. Build sequence

| Stage | Work | Exit condition |
| --- | --- | --- |
| 0 · API contracts | Create pinned test instances for the supported Sonarr and Radarr releases, plus the optional SABnzbd version. Build synthetic libraries, jobs and a controllable indexer with deterministic grab IDs. Capture sanitized fixture pairs for normal responses and failures. | Record exact version, settings, call, outcome, command history, download-client state and library file associations for each advertised capability. Mark unsupported or ambiguous combinations Observe-only. |
| 1 · Application foundation | Create the Go service, SQLite migrations, OS data-directory lock, startup setup, owner session, local data directory, logging, worker lifecycle, backups, health, platform packaging smoke and minimal authenticated console shell. | Each target can install, start, persist state, restart and restore. Competing process and filesystem/database errors visibly prevent dispatch. |
| 2 · Observe and ecosystem | Connect multiple instances, collect bounded health/queues/history/config/storage data, normalize with scope, correlate targets and job aliases, and implement incident detection. Ship the Cases, Incidents and Ecosystem views in Observe. | A full queue and a storm of storage or permissions errors remain understandable. Missing, stale and partial sources show correct states. Baseline resource budgets hold. |
| 3 · Assisted salvage and verification | Add read-only manual-import preview, exact identity and quality eligibility, case revisions, action proposal, preflight, durable intent, dispatch and command/library reconciliation. Admit one tested import path at a time. | Approved imports produce attributable file/target evidence. Stale approvals, unsafe cleanup combinations, timeouts and restarts are handled as specified. |
| 4 · Scoped replacement | Add hard-failure recognition, queue reject and release blocklist reconciliation, one permitted replacement search path, retry budgets and optional SABnzbd evidence. | An unusable job can be retired and replaced on eligible configurations. Client removal, actual blocklist, search, replacement grab and final import are separately tested. Unsafe, partial and shared cases remain reviewable. |
| 5 · Learning and automation | Add exact-scope evidence and sample eligibility, observation-only recommendations, counterexamples, replay, revocable learned rules, opt-in per-action automation and grouped Prowl notifications. | Patterns repeat safe decisions without widening policy. Replay makes zero upstream writes. Pause and incident holds survive restarts. Alert storms collapse and Prowl failures stay isolated. |
| 6 · Release qualification | Finish dashboards, action history, diagnostic bundle, capacity display, accessibility, maintenance flows and native/container release builds. Test appliance-size performance and real supported version pairs using controlled libraries. | Release report includes tested variants and deployment modes, measured resource usage, known limitations, backups and rollback, and every enabled action's evidence. |

Stages 1 and 2 can proceed in parallel with API fixture and lab work once the schemas are understood. Start mutating implementation only when the specific action contract in stage 0 has a reproducible verification plan. UI development can use the same fixtures in each stage. Do not wait until the final stage to exercise the normal path from browser to persisted records. Prefer a small early Observe beta and incremental Assist capability over one release that tries to certify every client and settings combination at once.

### First production scope

The first production release should include multi-instance Arr inventory, unattended recovery for certified cases, Assist for eligible but unconfigured automation, incident grouping, Prowl, audit logging, storage and version health, optional read-only SABnzbd diagnostics and native/container packages. Each mutating feature can be admitted independently. In a deployment where no write capability passes contract checks, the same product supplies diagnosis and explicit instructions in the owning application.

After that baseline, priority candidates are side-by-side policy comparison over retained evidence, estimates of the interventions avoided by each rule, a better incident comparison across shared services, and conservative root-capacity forecasts. Reserve work for further direct download-client adapters for a specific missing read-only fact. Every adapter adds a compatibility and support burden.

## 2. Test strategy

### Contracts and fixtures

Use three levels of evidence. A versioned API reference or released implementation defines an initial contract. Synthetic responses and fault injection validate Rescuarr's adapter and recovery behavior. An end-to-end test against that upstream version and configuration validates the actual effect.

For each promoted capability, preserve an acceptance record with exact upstream version and adapter version, relevant instance settings, client and protocol, initial queue/history/target file state, submitted payload, response or induced failure, subsequent upstream observations, measured verification windows and final case disposition. Remove API keys, filesystem paths and real library details from shared fixtures. Automated tests can use fictitious fixtures with stable IDs and explicitly simulated clock. Actual destructive tests use disposable jobs in an isolated lab. The compatibility report should say exactly which Sonarr, Radarr and optional downloader combinations were exercised.

| Layer | Required coverage |
| --- | --- |
| Decision tables | Table tests for reason precedence, policy intersections, invalid/unknown inputs, authority and time bounds. Property tests for no destructive plans on ambiguous, stale, shared, torrent or partially salvageable jobs. |
| API normalization | Fixtures for each exact supported version. Include missing optional fields, unknown enums, large or malformed responses, unexpectedly paged data, release history gaps, aliases, pagination drift and epoch changes. |
| Intent and reconciliation | HTTP simulation with lost responses, failures before and after acceptance, upstream partial effects, race with operator, delayed commands and retries. Force process exits at each transition. |
| Persistence | Constraint tests, transaction rollback, worker leases, SQLite lock/busy, full disk, interrupted migrations, WAL checkpoint growth, log retention and backup/restore. |
| UI | Browser end-to-end flows for install, sign-in, add instance, inspect case, approve stale/current proposal, pause, review incident, reset rule, configure notification and export. Keyboard and small-screen checks. |
| Upstream effects | Isolated instances confirm exact import paths and effects, native cleanup behavior, blocklist records, Arr search scope and result attribution, no eligible release, and duplicate built-in search suppression. |
| Distribution | Start the packaged artifact on each supported OS/architecture and both container targets. Test upgrade, backup and restore, service restart, permission errors, low disk, reverse proxy and disconnect/reconnect. |
| Security and privacy | Exercise state-change authentication, CSRF, credential masking, trusted proxy configuration, hostile text from upstream and scoped outbound destinations. Review exports, structured logs, notifications, command errors and backups for credential or path leakage. |

### High-risk scenarios

1. **Clean happy path.** A completed Sonarr episode has a unique matching candidate and clean proof. Arr imports it. A new file association and attributable history prove recovery. No replacement search runs. Repeat for Radarr.
2. **Wrong identity.** Two valid video files can match different episodes, or a movie has a plausible alternate version. Rescuarr holds or proposes human review. No automated import or deletion.
3. **Unsafe copy side effect.** Arr's tracked manual import performs cleanup despite its import mode. The configuration cannot qualify for a preservation-sensitive action. Test media and downloader job presence as separate facts.
4. **Quality and monitoring changed.** An operator changes the quality profile, monitoring or existing file between planning and dispatch. The action is re-evaluated and stops when the new facts do not support it. Preferences survive backup/restore in their respective owners.
5. **Partial season pack.** Verify target-level accounting when a pack has good and bad episodes, including an existing file for one episode. A partially recoverable pack cannot be auto-deleted, and any later search is narrowly scoped and version-certified.
6. **Duplicate library satisfaction.** The wanted file exists, including a correct comparison for an upgrade request. Record the actual satisfaction reason. Retain a separate disposition for the original queue job.
7. **Failed Usenet job with eligible replacement.** Verify exact identity and permissions, reconcile the Arr reject effects, identify the precise blocklist evidence and track the new grab through final import. Removing a queue row alone cannot advance the case.
8. **Failed blocklist.** Upstream removes a job while its history prevents blocklist insertion. Record the partial effect and stop dependent replacement automation. The original operation is never replayed on queue disappearance.
9. **Lost write response.** The HTTP connection dies after acceptance. Restart. Reconcile through command/history/library evidence and prove the wire saw no duplicate request. If effects are inconclusive, retain uncertainty and require a new decision before more writes.
10. **Other actor wins.** Arr's normal processing or a user action changes the job first. Record the external or ambiguous outcome. Exclude it from Rescuarr success and learning when attribution is uncertain.
11. **No replacement candidate.** The Arr search completes without a grab, finds a release that is out of profile, or repeatedly presents an equivalent release. Budgets and cooldowns stop the loop. The UI distinguishes search acceptance, search completion, no grab and no eligible result when known.
12. **Two instances share a client.** Identical numeric queue IDs in separate instances are distinct. Shared or ambiguous physical job ownership prevents job-wide writes. Verified independent HD and UHD targets remain separate.
13. **Infrastructure burst.** One unavailable or full storage target blocks many jobs while a different root works. Stop affected actions, group the incident, and resume after fresh healthy evidence. Unmapped capacity must remain unknown.
14. **Torrents and incomplete capability.** Preserve seeding and client ownership. Verify that protocol, client status, and preservation constraints gate all physical removal and side effects. Missing optional SABnzbd leaves the core dashboard functional.
15. **Prowl outage and recovery.** Induce provider rate limit, invalid key, malformed response and response loss. Recovery continues. A bounded outbox groups incidents and displays expired or uncertain deliveries independently.
16. **Persistent emergency pause.** Pause during a dispatch and restart. An already accepted command may finish. Other new intents cannot dispatch under the pause. The UI distinguishes the two.
17. **Learning regression.** Create enough eligible successful samples to promote an exact pattern, then introduce a verified negative or scope change. Suspend the rule. Historical replay displays the original and new decision with evidence and no writes.
18. **Local data failure.** Fill Rescuarr's data disk during intent recording and at reconciliation. Suppress further upstream writes, retain restorable state, and detect all relevant effects when storage returns.
19. **Upstream upgrade and restore.** Change adapter-visible version and simulate reused numeric IDs from an upstream restore. Gate writes until scope is verified. Treat restoring a Rescuarr backup as a new period of observed reconciliation.
20. **Pagination under churn.** Add and remove entries while collecting a large queue. The resulting partial data cannot support a false disappearance, wrong job group, or destructive action.

## 3. Rollout and promotion

1. Ship Observe as the first installation experience. Use a representative busy period and review the quality of decisions before enabling recovery. Show the operator the action coverage this installation can support and the reason for any hold.
2. Offer certified import in Assist. Release qualification exercises disposable cases for each advertised Arr version and settings combination. At setup, show example decisions from the operator's actual queue and let them enable qualified automation per action. Keep a direct link into Arr for cases the compatibility evidence cannot resolve.
3. Offer independent opt-in for verified import and each replacement action. Require a recent capability snapshot and qualifying observations. Physical removal carries its own authorization and stricter runtime requirements.
4. Release notifications after grouping and retry behavior are qualified. Include local counters for known recovery, replacement, manual assistance, and unresolved cases. Do not hide local results behind Prowl.
5. Treat schema and upstream version changes as capability changes. Upgrade in Observe for affected operations until the adapter can show their support. Leave diagnostics and unrelated verified features useful.
6. Publish measured budgets from [the performance plan](ARCHITECTURE.md#performance-targets), data-dir requirements, version coverage, migration notes, export formats, and the operator's pause and rollback procedure.

### Decisions to close during qualification

| Decision | Evidence required |
| --- | --- |
| Exact version and client compatibility | Successful pinned lab scenarios and matching released source. Establish per-action coverage instead of assuming API major version compatibility. |
| Import eligibility under force/manual semantics | Show a full-profile proof path and a verified preservation or explicitly scoped consumption contract. If either is unavailable, keep affected cases in review. |
| Replacement when staging capacity or size is unknown | Establish a defensible safe bound from relevant policy and capacity. Otherwise hold the action and show the missing fact. |
| SQLite driver and packaged platforms | Build and exercise the pinned driver/runtime across the actual OS matrix, including lock, power-loss-equivalent and database growth tests. |
| Numerical defaults | Reproduce timing and resource findings. Tune grace, poll and cooldown defaults in a documented test setup. |
| Long-term expansion | Prioritize cases where the first release saved meaningful operator time or surfaced recurring, resolvable gaps. |

## 4. Definition of done for production

- A supported installation with no direct downloader credentials can explain why jobs are waiting. With the necessary capability and permissions, an eligible stuck import resolves and a confirmed bad release can lead to a verified replacement.
- Operators can reproduce a recovery decision from its recorded evidence and policy revision. They can see when an outcome remains uncertain, what evidence would resolve it, and whether an incident or budget is holding the job.
- Qualification proves no duplicate dispatch on restart or ambiguous response, no auto-removal of torrents or shared/partially salvageable jobs, no cross-instance target confusion, and a reliable persistent pause.
- A one-instance setup is understandable and a mixed multi-instance setup remains responsive. Install, update, export, backup and restore are exercised in supported packaging modes.
- The release includes observed compatibility limits, redaction checks, accessibility checks, performance results and a traceable account of which mutating capabilities are enabled.
