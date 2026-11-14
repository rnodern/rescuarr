# Rescuarr web console design

The console should let an operator understand the system in a minute, leave routine recovery running unattended, and resolve exceptional cases without reconstructing their history across several apps. The home screen prioritizes decisions and incidents. Each proposed action has an explanation, a list of effects, and a visible verification path.

[Architecture](ARCHITECTURE.md) · [Recovery policy and state](RECOVERY_DESIGN.md) · [Integration contracts](INTEGRATIONS_DESIGN.md)

## 1. Navigation and information hierarchy

| Destination | Operator question | Main components |
| --- | --- | --- |
| Overview | What needs me right now? | Mode, pauses, incident summary, attention queue, active recoveries, verified activity, instance freshness |
| Cases | Which downloads have a problem? | Filtered list, grouped jobs, verification status, next action, closed cases |
| Ecosystem | Is something wrong upstream? | Instances, clients, storage roots, path mappings, provenance, incidents |
| Activity | What happened and who initiated it? | Searchable audit trail, action outcomes, notification delivery, diagnostic events, redacted export |
| Learning | Which proven fixes are being reused? | Patterns, qualifying outcomes, match explanations, replay, pause and reset |
| Settings | Where can Rescuarr act and how? | Connections, policies, notifications, access, retention, backups and diagnostics |

The desktop sidebar contains six destinations. On mobile use a labelled navigation disclosure with quick access to Overview and Cases. Every view shows the current operational state in the header. Display **Observe**, **Assist**, **Autonomous**, or **Mixed across instances** and link to the effective policy. Global pause, instance hold and incomplete start-up verification take precedence over the mode badge. Show whether reads continue while actions are paused.

Count cases, jobs, targets and incidents independently and label their units. A failed multi-episode download contributes one attention case while all affected targets remain visible. Within filtered views, scope counts to the same filters as the rows or label broader counts explicitly. URL filters and cursor pagination make views shareable within the authenticated console. Built-in views cover **Needs you**, **Waiting for a fix**, **Running**, **Watching**, **Recovered** and **Closed**. Named views store local filter and column preferences. Bulk selection supports snooze and redacted export. Approvals and service writes are per case and per proposed action.

## 2. Setup and activation

### Guided first use

1. Set up an owner session through a short-lived local setup token and choose an owner credential. Authenticate the console after setup, including on loopback. Require an explicit HTTPS reverse proxy for LAN use. Show a short note beside the base address that explains the proxy and path prefix. A bootstrap screen persists valid staged data and can resume after refresh or restart.
2. Add a Sonarr or Radarr connection. Ask for display name, base URL as seen by the Rescuarr process, and API key. A reverse-proxy path is part of the URL. The form masks credentials, omits values from URLs and telemetry, and never redisplays a saved secret. If the form discovers a problem, keep the safe inputs editable.
3. **Test connection** reads status and capability metadata on demand. Show identified app/version, authentication result, and independent coverage for queue, storage, health, and other supported reads. Give next steps for DNS, refused connection, TLS validation, auth, wrong app, base path and incompatible response. A container deployment gets the helper text “This address is looked up inside the Rescuarr container.” Network checks preserve existing TLS validation.
4. **Save and start observing** schedules a snapshot. Display each area's last successful timestamp and its freshness. When queue reads succeed while storage or history reads fail, the operator can browse queue data and see which decisions require more evidence. Land on Overview when one instance returns a queue observation.
5. Offer add-another-instance. Give distinct labels to overlapping Sonarr or Radarr instances such as “Sonarr HD” and “Sonarr UHD.” An identity collision opens a review and holds affected writes until the relationship is resolved. Multiple configured URLs can refer to the same service.
6. Offer optional SABnzbd diagnostics, Prowl and storage group assignments after the first snapshot. Basic recovery uses Arr APIs. Describe whether a storage group is confirmed or an operator declaration and make unknown capacity explicit. None of the optional integrations prevent initial observation.

A setup test has its own expiry. The last valid connection remains in effect until the replacement is validated or the owner explicitly saves it in an observe-only state. An Arr identity or relevant configuration change starts a new evidence epoch. Warn about the need to rebuild confidence in affected patterns and show outstanding write reconciliation status.

### Activation and dry run

The policy matrix shows rows for instances and columns for actions. Separate qualified import, retirement of an obsolete queue entry, blocklist, replacement search and eligible Usenet client removal where Arr supports separate control. If upstream couples effects, show the combined operation or disable that combination. **Off**, **Review**, and **Automatic** carry short descriptions. Derive the instance mode from effective action policy.

For each Automatic setting, display:

- Exact scope and allowed effects, including removal of download data when applicable.
- Proven capability for the Arr version and protocol. Disable writes that await validation.
- A replay of the proposed policy against retained observations. Show would-act, would-hold, and unknown counts with coverage. Replay uses no external writes, and results carry the evidence window and policy revision.
- Per-target and per-instance limits, incident gates, and effective policy where scopes overlap.
- Cases whose projected decisions change, including at least one hold if present.
- The Arr policy that governs quality and where it can be edited. Label inference when profile detail is incomplete.

The confirmation page compares old and new effective policies and lists affected scopes. Reauthenticate before expanding destructive policy. Persist the revision and audit it. Reevaluate cases on fresh evidence under the new policy. Case ambiguity, stale state, capability and capacity holds continue to apply.

Provide **Pause all actions**, **Pause this instance**, and action-specific pause. Apply the gate to new dispatch at once. Show whether an in-flight Arr request can still finish. Acknowledging an incident quiets repeated alerts while monitoring and guards continue. A bounded case snooze hides the case from attention views and holds further automatic handling until expiry or the explicitly selected qualifying evidence change. Resuming schedules reevaluation and retains attempt budgets.

## 3. Overview

This layout sketch uses synthetic content.

```text
Rescuarr                                      Mixed · Pause actions
Overview   Cases   Ecosystem   Activity   Learning   Settings

Storage incident · TV volume                                    Open incident
16 cases held across Sonarr HD and Sonarr UHD. Last checked 18:42.

Needs you · 3                           Recovering · 4
2 ambiguous imports                    2 checking Arr import result
1 expired retry budget                  2 waiting for replacement

Recent outcomes                         Connections
18:35  Imported · Episode 4             Sonarr HD     Read 42 s ago
17:58  Replacement imported · Movie    Sonarr UHD    Read 38 s ago
16:20  Existing file satisfied target   Radarr        Read 51 s ago
                                        SABnzbd       Diagnostics off
```

Cards link to their filtered lists. Clearly distinguish cold start, true queue empty, queue read failure, partial inventory, and filtered-to-zero. Retain a useful stale snapshot with age, last reliable source and error category. Exclude unknown conditions from healthy counts.

The **Recovery activity** view covers a rolling seven days. Count verified original-file recoveries, verified replacements, already satisfied targets, new review items, and incidents that held work. Show denominators, source coverage and attribution on drill-down. A verified Arr import while Rescuarr was watching is an external recovery. If reporting estimated bytes reused, require known source size and verified original-job attribution, show the estimate method, and exclude ambiguous, partial, and duplicate records. An optional **Since your last visit** view uses a local audit cursor to show new incidents, verified outcomes and superseded decisions. Suppressed notifications still have an audit trail.

## 4. Cases and investigation

### Queue and target grouping

Default sort places open external-repair incidents first, then manual decisions, unknown write outcomes, active actions, held cases and closed outcomes. Within a bucket use oldest due first and a stable ID for ties. Filters include instance, media type, root/storage group, protocol, state, cause, action, age and provenance where verified. Search title, episode or local case ID. Search results exclude unredacted paths, arbitrary remote logs and secrets.

Desktop columns include title and affected target count, instance, cause, state, next decision, age and last observation. Mobile presents labelled fields in a stacked row. A multi-episode job opens one case with an expandable target table. Each target shows its own **Satisfied**, **Eligible**, **Needs a match**, **Held**, or **Waiting for verification** status. Whole-job effects carry a separate hold. Link other claims on the same job and cases in the same incident.

Represent workflow phase, verification, attention and policy gate as separate fields in the view model. For example, an operator can see “Replacement in queue,” “Automated removal held” and “Waiting for target file verification” on the same case. Expose confidence as evidence strength where the recovery spec supports it, with labels and facts rather than invented percentages.

### Case detail

The header presents title, media type, affected targets, Arr instance, source protocol, phase, incident and last reliable observation. Show upstream event times and local receipt times separately when that distinction affects attribution.

1. **What happened** displays the reported Arr warning, observed state, Rescuarr's current diagnosis and the main uncertainty. Evidence expands inline with source and timestamp.
2. **What you asked Arr for** displays title, episodes, wanted or monitored state, current file and quality, profile, upgrade state, and decision-relevant format or language evidence where available. Report “No existing file” only with supporting evidence. Link to Arr for governing profile settings. If a deep link is unsupported, use the instance homepage.
3. **Where it came from** shows the linked grab, release, client, category, reported indexer and download ID. Give each value a source and timestamp. Missing and conflicting evidence read **Unknown** and **Disputed** respectively.
4. **What can happen next** lists allowed proposals and held effects with evidence, exact scope, missing prerequisites, remaining attempt budget and expected verification. Typical copy includes “Import the one matched episode,” “Wait until the storage root is reachable” and “Blocklist this verified failed release, then let Sonarr search again.” Where a policy permission is missing, link to the effective setting.
5. **Timeline** merges observations, classification, proposals, intent, dispatch, verification, outcome and notifications. Keep the decision-time evidence and policy revision visible after later changes. Distinguish an operator, a recovery rule, and an observed external change.

Candidate media details may include basename, size, reported quality, duration, root-relative path alias and target mapping where the integration knows them. Display redacted path aliases and bounded redacted upstream log excerpts even in expanded evidence. Paths returned by services are inert text. Use local title and episode metadata so the case view adds no third-party asset requests.

### Action review and status

The action button opens a review page pinned to a server-issued, single-use proposal revision. The **Effects** table covers library, Arr queue, download-client job, download-client data, blocklist and search. Each effect reads **Will change**, **May change upstream**, **Preserved**, or **Unknown** with a short explanation. Unknown destructive effects suppress submission. Display the deadline supplied by the recovery policy and give a **Refresh proposal** action when it expires. Use exact button text such as **Queue import for Episode 4** or **Remove failed Usenet job and blocklist release**.

For proposals that include irreversible client data removal, require acknowledgement of that effect and identify the job and release. Allow correction only through currently valid Arr candidates and eligible policy, then produce a fresh proposal. Complex manual mapping, unsupported import and deliberate quality override route to Arr. For seeding torrents and ambiguous, shared or partly salvageable jobs, display the physical-removal hold and any individually eligible actions. Copy-mode import proposals also show a source-preservation hold unless the adapter has verified post-import cleanup behavior. An operator can inspect the preview and open the native Arr workflow in that situation.

Submission creates an intent and redirects to its timeline. Repeat local requests with the same request ID resolve to the same intent. Server-side preflight verifies current evidence and policy before dispatch. If the proposal, evidence, target, or permission has changed, present a conflict page with the meaningful differences and a fresh proposal. A new pause applies before dispatch. After dispatch, the console can pause follow-up steps and keep reconciling the existing request.

| Runtime state | Copy and interaction |
| --- | --- |
| Queued | “Import scheduled.” Show next step. Offer cancel only while dispatch is cancellable. |
| Acknowledged by Arr | “Sonarr accepted the import request.” Show verification pending. |
| Response lost or outcome unknown | “Checking whether Sonarr accepted the request.” Allow read-only recheck. Disable repeat execution. |
| Partially verified | List observed effects separately, such as “Queue item disappeared” and “Blocklist still unconfirmed.” Explain why follow-up is held. |
| Import verified | Show target, observed file/quality, verification time and source. Preserve separate cleanup status. |
| Replacement grabbed | “Replacement downloading.” Link original and new jobs. The target stays open until the file qualifies. |
| External outcome | “Arr imported this while Rescuarr was watching.” Show evidence and attribution. |
| Operator needed | State the smallest missing decision, link to Arr or incident, and show next reevaluation when scheduled. |
| Verification expired | State the last reliable effect, last checked time and evidence still required. Keep the unresolved attempt on the timeline. |

## 5. Ecosystem and incident views

Instance pages show observed app/version and identity, mode, freshness by API area, write eligibility, last inventory refresh, queue coverage and scheduler state. A partially connected instance can read “Queue current, storage stale, import verification paused.” Unsupported versions show observed read capabilities and reasons for held writes.

- **Health** separates Arr warnings from Rescuarr incidents. Group related evidence by confirmed root, client and instance, and link affected cases.
- **Storage** shows root, scope, observed free and total bytes, evidence source and time, confirmed shared volume relationship, and applicable reservation estimates or headroom. Sum volumes only when identity is confirmed. Keep unknown remote capacity explicit and separate from free space for the Rescuarr database.
- **Download clients** show Arr-reported name, type, protocol, enabled state and health. Optional SABnzbd adds read-only queue/history diagnostics for matched jobs. All job effects still originate in Arr.
- **Provenance** groups verified observations by instance, indexer, client or root. Show sample sizes, missing coverage and successful observations alongside failures. Use evidence such as “8 of 11 classified failures shared this root” and avoid attributing causality to an indexer from a release count. No indexer controls.
- **Logs** shows bounded warning or error excerpts near the selected case or incident. Attribute and timestamp each redacted snippet. A privileged support export can include the redacted subset and coverage. No general-purpose upstream log browser.
- **Activity** is the audit view scoped to the instance. Browser refresh reads local snapshots.

Incident detail leads with scope, current health, first and last observations, affected jobs and blocked actions. List the shortest checks appropriate to the evidence, such as “Open Sonarr root folders and confirm that the TV mount is writable.” Link a filtered list of affected cases. **Recheck** schedules a bounded read through the shared polling coordinator and respects backoff. Concurrent requests share the next result. Show last observed repair evidence and the earliest resume if policy requires consecutive good observations. Acknowledgement and snooze operate on alerts and visibility. Actual release from the gate depends on the recovery evidence and policy. On resolved incidents, show counts ready for reevaluation, still needing review, and already recovered upstream. Link repeat episodes without losing the original event history.

## 6. Learning, audit and notifications

### Saved recovery patterns

Present deterministic learning as **Saved recovery patterns**. List scope, cause, treatment, qualifying evidence, verified outcomes, contradictions, first and last observations, status and version. Distinguish collecting evidence, eligible for recommendation, eligible under policy, held, disabled and invalidated. Show the actual evidence threshold and excluded samples with reasons. Helpful feedback is stored separately from independently verified recovery.

Pattern detail includes the rule version, scope, sample outcomes, fields that must match, and evidence that would invalidate future use. The case view's **Why this pattern matched** disclosure shows exact field matches and current blockers. **Replay recent cases** evaluates retained evidence against a pinned rule and policy without dispatch. Show its coverage, proposed outcomes, and records that cannot be replayed. Permit pausing or resetting an exact pattern with an audit reason. Turning learning off removes its influence on current decisions while preserving recorded outcomes. Feedback distinguishes wrong identification, missing evidence, upstream behavior changes and a genuine failed treatment.

### Activity and support

Filter the audit by case, actor, result, instance, action, incident and time with a stable time/event cursor. Render local times with absolute timestamps available and use UTC in exports. Diagnostic events include snapshot coverage, capability changes, polling and backoff, maintenance and provider response categories. Correlation IDs join intent, attempt, remote observations and verification. Redact keys, cookies, raw upstream URLs, unredacted remote request bodies and unrestricted paths before presentation or export.

Offer **Copy summary** on cases and incidents. Preview the fields first and use a support-safe default that excludes title and release identifiers. Supply a selectable-text fallback. Exports enforce a date limit and record schema and redaction metadata. Retention settings show the minimum evidence preserved for attempt budgets and verification. Explain how reducing longer evidence retention can invalidate pattern eligibility or prevent replay.

### Prowl notifications

Offer test delivery, filters, quiet hours, timezone and a compact digest. Default enabled categories after setup should emphasize new incidents, decisions requiring attention and unresolved writes. Allow opting into verified results and recovery notices. Group repeat outage notices and send a resolution notice only when recovery evidence supports one. Prioritize urgent conditions without notification storms. Delivery status separates outbox queued, provider accepted, authentication failure, quota, transient failure and uncertain. Prowl acceptance is evidence of provider receipt. Handset delivery remains unknown.

Default message payload excludes titles, release names, private paths, indexers and API keys. Opt-in title detail shows the exact representative payload in preview. Include a case link only when the owner configures an explicit console address reachable from the device. Links always require normal authentication. Test notifications are generic and rate-limited. Notification outages appear in settings and activity while case processing remains available.

## 7. Access and settings

| Group | Controls |
| --- | --- |
| Connections | Add and test, verify identity and feature eligibility, rotate key, pause, archive, review replacements |
| Recovery | Instance/action policy, overrides within ceilings, retry limits, grace windows, capacity thresholds, replay |
| Integrations | Optional read-only SABnzbd, Prowl, configured console URL |
| Access | Owner credential, sessions, proxy status, security audit |
| Diagnostics | Logging verbosity, retention, disk budget, backup status and redacted exports |

V1 uses one owner role. Apply authorization at all routes including fragments, counts, status, downloads and mutation confirmation. A later multi-user release can distinguish owner configuration, operator approval and viewer read access once scoping is implemented end to end. Avoid shared session URLs. On archiving an instance, preserve outstanding reconciliation records and stop new work. Prior events retain the prior epoch and display label. Credential fields replace stored values only on explicit submission. Outgoing Arr links are built from a validated configured base and known local routes. Escape all service-controlled text.

## 8. Rendering and route contracts

Use Go server templates, semantic HTML, one static stylesheet and a small deferred script. Embed assets in the binary. Use system fonts, locally bundled icons with text labels, adaptive dark and light themes, and a usable 320 CSS-pixel viewport. Login, onboarding, investigation, pagination, evidence disclosure and approvals work without JavaScript. Progressive enhancements cover local status updates, clipboard and disabling duplicate pending submits. Use CSS semantic status treatments without relying on color alone.

Page rendering reads current local state in a short DB transaction. Conditional refresh uses ETag and If-None-Match against an authorization-scoped snapshot revision. Use a compact local status fragment at 20-second intervals on active pages, pause when hidden, slow or stop for settled cases and refresh on visibility change. Re-fetch only affected content. Avoid replacing focused elements or active forms. Multiple tabs share upstream snapshots. Consider server-sent events only if measured freshness needs them, as invalidation hints with conditional polling fallback. [HTML standard on server-sent events](https://html.spec.whatwg.org/multipage/server-sent-events.html)

These routes belong to Rescuarr. The browser sends server-issued local IDs and approved revision. Handlers resolve typed proposals and current policy and never accept raw Arr payloads, paths, operations or provider keys in action URLs.

| Method and path | Contract |
| --- | --- |
| GET / | Overview, grouped counts, holds, verified outcomes, mode and freshness |
| GET /cases | Bounded filtered list. Parameters include view, instance, state, cause, q and after. Cursor tied to stable filters and sort. |
| GET /cases/{caseID} | Case, job and target states, proposals, holds, evidence and paged timeline |
| GET /cases/{caseID}/review/{proposalID} | Review of current effects, evidence and policy revisions, expiry and prerequisites |
| POST /cases/{caseID}/actions | Submit proposal ID, revision, local request key and CSRF token. Redirect to existing or new intent. |
| GET /actions/{intentID} | Effects, dispatch state, attempts, verification and outcome |
| POST /actions/{intentID}/cancel | Cancel still-cancellable intent and audit outcome |
| POST /cases/{caseID}/snooze | Set or clear bounded local snooze with revision and reason |
| GET /ecosystem and GET /instances/{instanceID} | Paginated topology and per-instance local evidence |
| GET /incidents/{incidentID} | Timeline, gates, scope and recovery evidence |
| POST /incidents/{incidentID}/acknowledgements and POST /incidents/{incidentID}/rechecks | Local acknowledgement or bounded read request |
| GET /activity and GET /activity/export | Paged audit and authorized redacted export |
| GET /learning and GET /patterns/{patternID} | Pattern status, scope, rule version and paged evidence |
| POST /patterns/{patternID}/replays and POST /patterns/{patternID}/decisions | Bounded local replay and separately authorized pause/reset |
| GET /settings/{section} and POST /settings/{section} | Revisioned settings forms and typed change preview |
| POST /settings/connections/tests and POST /settings/notifications/tests | Explicit bounded upstream read or test notification |
| POST /controls/pause and POST /controls/resume | Scoped gates and reevaluation scheduling |
| GET /fragments/status | Conditional authenticated HTML fragment, optionally scoped to a case or incident |

Safe GET requests do not change state or schedule jobs. POST uses CSRF, same-origin and authorization checks. Successful HTML mutations use Post/Redirect/Get. A stale revision returns a conflict with changed fields. A policy-gated action returns its reason and current state. An expired session shows login for top-level navigation and an authentication result for fragments. Public liveness endpoints omit media and operational details. Reads and exports use bounded page limits. Replays and connection tests expose background status and expire temporary output.

## 9. Accessibility and performance criteria

- Meet WCAG 2.2 AA. Test landmarks, table headers, visible focus, names, error association, keyboard navigation, reflow, zoom, reduced motion and contrast. Announce meaningful changes in a polite live region while leaving poll ticks silent. [WCAG 2.2](https://www.w3.org/TR/WCAG22/)
- All review flows work by keyboard. On form failure focus the error summary. Move focus to the next review heading when opening a review. Returning to a list retains filters and logical row position. Browser Back recovers operator context.
- Keep effect labels and hold reasons visible on narrow screens and at enlarged text sizes. Use responsive tables with preserved labels, comfortable touch targets, word wrapping for long technical IDs, and horizontal overflow only for detailed evidence that needs it.
- Aim for under 60 KiB compressed combined CSS and JavaScript on first load and zero third-party asset requests. Page the case, audit, log and pattern lists, initially 50 items and at most 100. Scope summaries to indexed projections. Use the architecture's reference workload and response-time gates for local projections.

## 10. Acceptance scenarios

| Scenario | Expected experience |
| --- | --- |
| Wrong hostname from a container | Connection test names the service-side resolution failure and keeps the address editable. |
| Queue healthy, capacity unknown | Queue remains browsable and capacity reads unknown with timestamp. Applicable writes show the missing evidence. |
| Thirty imports fail after a root unmount | One storage incident lists thirty held cases. Acknowledgement leaves the operational gate active. Recovery shows what will be reevaluated. |
| Two Sonarr instances have the same numeric episode ID | Results and policies remain separate. An approval for one instance cannot affect the other. |
| Six-episode job with four known matches | One case lists six targets, their independent eligibility and a whole-job removal hold. |
| Existing file satisfies a target | Explain what Arr already has, what queue effects are available and how the library file is protected. |
| Stale review after policy changes | Submission returns a conflict with the policy delta and dispatches nothing. |
| Arr accepts import but the response times out | Timeline shows unknown and reconciliation. Refresh is read-only and repeat execution is unavailable. |
| Blocklist confirmed while client removal is unclear | Present each effect independently. Keep unresolved work visible and hold duplicate writes. |
| Replacement appears in queue | Show original and replacement jobs. Increment recovered count only after qualifying file verification. |
| A learned treatment later contradicts evidence | Show the contradiction in that pattern's history and offer pause/reset and replay. |
| Prowl key revoked or quota exhausted | Show reason and bounded retry status. Recovery remains visible and usable. |
| Keyboard and screen reader at 320 px | A user can inspect every effect and reason, navigate the case and approve an eligible proposal. |
| JavaScript off or multiple tabs open | Core workflows operate and upstream polling stays shared. |
| Export or fragments without authorization | No private title, path, provider key or scoped status appears in response or URL. |
