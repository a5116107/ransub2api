# Release matrix

The release workflow builds the frontend once, then runs each configured Go target on its own Linux runner. `CGO_ENABLED=0` permits cross-compilation. The target matrix is read from `.goreleaser.yaml`, including its exclusions; simple releases select Linux amd64 only.

Each build uses GoReleaser OSS in snapshot mode with the selected release version and one target. Archive naming, bundled files, Go flags and release templates remain in the existing GoReleaser configurations. Every archive is accompanied by its source commit, target, version and SHA256. The publishing job verifies the complete matrix before building images or publishing. It uses GoReleaser's `extra_files` support to publish existing archives and checksums, with builds disabled. No Pro license is needed.

Go caches are isolated by target and refreshed on each source commit, with fallback to the preceding target cache. Save uses the original restore key, even if a build hook changes `go.sum`. Matrix jobs upload uniquely named artifacts. The publishing job extracts only the regular Linux binary from each verified archive and restores its executable permission before constructing Docker contexts. QEMU remains limited to runtime-image instructions. DockerHub images are omitted when its credentials are absent; GHCR is always retained. Simple mode still publishes only the amd64 GHCR image and the simple release description.

All build jobs use the commit resolved by `prepare`, including a manual release's selected tag. Helper scripts come from the workflow revision and are passed as a run-local artifact, so older application tags do not need to contain the new scripts. The workflow serializes release runs to prevent simultaneous updates to moving image tags.

## Validate without publication

From a branch containing this workflow:

```bash
gh workflow run release.yml --ref <branch> \
  -f tag=<branch> -f dry_run=true -f simple_release=false
```

A dry run builds all selected archives and both runtime images, verifies artifact provenance and produces the final checksum file. It exports images locally as OCI archives instead of pushing them. It skips registry logins, GitHub Release publication, DockerHub description updates, Telegram notifications and VERSION synchronization. Test the simple path separately with `simple_release=true`.

Dry-run artifacts are available in the Actions run, including `release-dry-run-report`. Compare job start/end times, GoReleaser's build duration and cache restore results. Do not present an initial cold-cache run as a warmed-cache benchmark; publishing network time is not measured by dry runs.

Helper checks:

```bash
python -m pip install -r .github/release-tools/requirements-release.txt
python -m unittest discover -s .github/release-tools -p 'test_release_matrix.py'
bash -n .github/release-tools/release-images.sh
```


## Production verification boundaries

Production smoke checks send only valid, bounded positive business requests. Run malformed input, orphan tool output, injected upstream failures and other expected-error cases in local tests or disposable isolated rehearsals, not against the production URL with a real user's key. An expected rejection still creates a real production error record and can mislead operators.

Identify operator HTTP probes with `RanSub2API-Production-Smoke/1.0` (tool round trips keep their existing tool-verification identity). Preserve returned gateway/client request IDs and the probe ID in sanitized results; never store keys or conversation bodies. A failed positive probe stays failed and is investigated before another paid invocation. Do not hide errors by User-Agent, loosen request validation or delete historical error records to make a smoke check appear clean.

## Automatic custom stable channel without test CI

The user-owned public channel is `a5116107/ransub2api`. Its `production` branch retains custom history. `sync-upstream.yml` tracks stable `ranxi2001/sub2api` releases every six hours. `upstream/stable` mirrors the pinned application sources with the owner's `.github/workflows` directory retained. Each tracking commit includes the raw upstream commit as a parent and in `Upstream-Commit`; all non-workflow files must match it exactly. This avoids requiring workflow-write credentials in Actions or importing upstream CI. Never use this tracking branch as a production artifact. The unrelated `a5116107/bps2api` fork is unchanged.

A one-time local `python -B .github/release-tools/sync_upstream.py --bootstrap-tracking` transitions an existing raw tracking branch to the owner workflow policy. It uses existing owner credentials, a normal fast-forward push and a `[skip ci]` commit, without storing a long-lived token in GitHub. Subsequent scheduled pushes use the ephemeral `github.token`; the script verifies unchanged workflow files before pushing. Upstream history rewrites stop synchronization. Git failures show sanitized stderr.

The helper preserves an unmerged `integration/upstream-stable` candidate. If that candidate is already merged, the next release is proposed from the new tracking tip instead of creating an empty PR from the old candidate. Review semantic conflicts and run local regression, migration and rollback checks before promotion. Scheduled upstream synchronization does not merge, compile, publish or deploy. Imported test workflows retain manual triggers.

After accepted application changes reach the owner's `production` branch, `release.yml` automatically selects the next `X.Y.Z-ransub.N`, builds the embedded frontend and Linux amd64 archive, verifies checksums/source identity and the packaged executable, publishes its GHCR revision image, then publishes the public stable Release and promotes the image's `latest` tag. The frontend is type-checked and built directly; the package's test-containing build alias and all test CI are excluded. Feature/tracking branches and pushed tags do not trigger production publication. This does not merge upstream code, deploy, restart services or send announcements.

A 15-minute scheduled check recovers skipped push events (including `[skip ci]` and bot pushes). GitHub schedules can be delayed. It reads `production`, compares application sources with the latest published custom tag, and does nothing for already-published, documentation/tooling-only or backend Go-test-only (`*_test.go`) changes. Runtime source, embedded data, migrations, resources and dependencies still count as application changes, including when changed alongside tests. Versions come from the committed VERSION base plus the next available custom revision; promotion rejects downgrades, unrelated histories and stale production builds. Concurrent release runs are serialized. Partial draft uploads may be repaired only for the same unpublished source/tag; published release assets remain immutable. If production advances during upload, the draft stays unpublished and its image does not become `latest`.

The same check compares the stable revision image with `latest` and repairs a missing/stale image pointer without rebuilding or issuing another Release. This recovers an interrupted image promotion after Release publication; matching images are left unchanged.

The online admin updater continues reading this repository's public latest Release. It caches update checks for 20 minutes; **Check for updates** forces a fresh check. Installation and service restart retain the existing explicit one-click controls. A successful Release is distinct from a verified production installation. Keep migration/rollback checks local before promoting application code.

Manual local publication remains available for recovery. Build from committed clean source, then publish its exact tag and archives:

```powershell
python .github/release-tools/release_matrix.py local-build --simple --output H:/Root/DevCache/releases/ransub2api
git tag -a vX.Y.Z-ransub.N -m "Custom stable release"
git push origin vX.Y.Z-ransub.N
python .github/release-tools/release_matrix.py local-publish --input H:/Root/DevCache/releases/ransub2api --notes NOTES_PATH
```

`--simple` builds a real Linux amd64 archive including the embedded frontend; it is not the old image-only simple release. Other targets remain unavailable until built and verified. Publication first creates a draft and compares GitHub-calculated asset digests before making it latest. Failures leave the draft unpublished. No GitHub credential is included in the package or required by public update downloads.

Local builds select the tracked `vite.config.ts` explicitly and write frontend assets into one fresh, task-owned directory under the release output. The existing Go build uses an `-overlay` mapping to embed only those assets while hiding historical development `backend/internal/web/dist` files; it does not delete or modify that development directory. The fresh assets and overlay are reclaimed after the build, including compiler failures. Do not work around a protected output directory by embedding its accumulated resources.

`verify`, image contexts and publication enforce the online updater's unchanged 500 MiB archive and extracted-binary limits. An oversized, empty, missing, duplicate or linked executable is rejected even when its checksum and source manifest match. Keep the runtime decompression bound; investigate stale embedded inputs instead of raising it to publish an inflated build.

To recover a missing GHCR runtime image for a locally published release, manually dispatch the existing Release workflow with `tag=vX.Y.Z-ransub.N` and `prebuilt_only=true`. This path verifies the downloaded archive against its source commit and packages it without compiling or running test CI. Confirm public image pull access before pinning production to its digest. Container update binaries must persist across recreation; keep the previous image and database/data backups until deployment acceptance is complete.

## Local drained upgrade and rollback rehearsal

Use the existing CLI with a saved, verified Linux amd64 custom release and a locally built candidate:

```powershell
python .github/release-tools/release_matrix.py local-rehearse `
  --baseline H:/Root/DevCache/releases/ransub2api/2.19.2-ransub.2 `
  --candidate CANDIDATE_BINARY --candidate-sha CANDIDATE_SHA `
  --scratch H:/Root/DevCache/tmp/sub2api-v221-upgrade `
  --output backend/tmp/protocol-cache/upstream-v2.21.0-upgrade-rehearsal.json
```

The candidate must be Linux amd64 with its full source SHA in `-version`. The baseline manifest/archive hash and both binary identities are checked before creating the fixture. The command requires a local Docker daemon and already available `python:3.11-slim`, `postgres:17-alpine` and `redis:7-alpine` images; it never pulls images or accesses production. Override the image options only with compatible locally installed images. The runtime image needs Python 3.11 or later for the standard-library loopback HTTP client and `/proc/1/exe` digest check.

An internal Docker network and tmpfs-backed data isolate the fixture; no port is published. Synthetic admin/account data completes normal fixture onboarding, without changing the production guard or user acknowledgements. Stages are baseline → stopped baseline → candidate → stopped candidate → old binary rollback → stopped rollback → candidate again. Each stage verifies the actual running binary hash, health and authenticated API access. Checks cover unchanged old migration checksums, account credentials/pricing and paused performance-monitor state, a legacy-shaped save retaining the candidate's TPS target, an ordinary account edit after rollback, and idempotent re-upgrade.

This proves a serial, drained rollout on newly initialized disposable data. It does not prove a restored production-data upgrade, concurrent mixed-version isolation, upstream model quality or cache lifetime. Retain those acceptance items separately. Resource IDs and outcomes are written to the requested result; cleanup verifies ownership before removing only its containers/network and extracted baseline. Unknown ownership, Docker errors and failed cleanup remain failures with retained evidence. The caller owns the candidate binary and removes it after verification; release archives are retained.

### Optional mixed-version control-plane check

Add `--overlap-config` to the same `local-rehearse` command to start an independently
identified baseline peer while the candidate is running. Both use the fixture's
PostgreSQL and Redis, with no published ports or access to real upstreams. The
normal drained upgrade/rollback stages remain unchanged. This option tests an
overlapping **management** phase; it does not authorize a rolling production rollout.

The check reads the persisted scheduling JSON, rather than trusting API defaults.
It verifies an old-node save, an old-client-shaped candidate save, two concurrent
saves, and an old-node account edit. Cache weight, TPS, session protection,
credentials, account pricing and paused monitoring fields must survive. A mismatch
returns a nonzero exit with `config_overlap.status=incompatible`, `failed_stage`
and the affected field names. No credentials or account contents are written to
the result. Existing peer names are protected before creation; cleanup uses
captured resource IDs. PostgreSQL uses an explicit `PGDATA` under fixture tmpfs,
including with images whose default data layout changed in PostgreSQL 18.

`config_overlap.status=compatible` covers only the tested source pair and
`priority_configuration_and_account_fields`. The broader
`mixed_version_isolation_verified` remains false: request ownership, shared
capacity, background tasks, non-text capabilities and cache lifetime need their
own evidence. In particular, a `2.21.0-ransub.9` writer drops the newer
`cache_weight` field; drain and retire that writer before managing newer policy.
An old-client save through a new server is not proof that an old binary is safe.

### Optional two-version Responses HTTP and native WebSocket request check

Add `--overlap-requests` to that same command; it can be combined with
`--overlap-config`. It starts an isolated upstream on the internal network and
uses the verified baseline and candidate binaries against the fixture's shared
PostgreSQL and Redis. No real supplier is contacted and no port is published.
The two synthetic accounts explicitly select native Responses HTTP and WebSocket
passthrough, avoiding an unrelated automatic capability probe changing the
protocol under test. WS mode routing is enabled only in the isolated fixture.

Two accounts in different product groups share one upstream Key with a total
ceiling of **1**, while retaining their individual ceilings of 2. Both directions
(baseline holding capacity / candidate requesting it, and the reverse) must
return the specific local admission 503 without another physical upstream call.
After completion, the other node must recover. Successful streams must have one
complete terminal and exactly one usage, billing claim and quote, attributed to
the original account, product group and fixed selling rate. The synthetic rates
0.12 and 0.15 are fixture examples, not supplier procurement prices.

HTTP client cancellation does **not** prove that upstream generation stopped:
the existing service drains upstream for final usage. In both directions the
check therefore requires capacity to remain occupied while that call is active,
with no replay. Only after the upstream terminal may capacity return; the drained
call must settle once and another request must succeed. Rejected requests must
create no usage, claim or quote. A mismatch remains a failed result, not a skipped
or relaxed assertion. Startup acknowledgement and failure logs always address
the particular node being started, not whichever node is currently primary.

`request_overlap.status=compatible` covers only
`responses_http_shared_key_capacity_cancellation_and_settlement` for that exact
binary pair. The same run additionally records `websocket_overlap`: both node
directions must enforce the shared ceiling across WS/WS, WS/HTTP and HTTP/WS,
with no extra physical generation or billing for six rejected requests. Each
recovery uses two turns on one real native WebSocket, preserving the preceding
response ID and verifying a unique usage/claim/quote per upstream turn, its
account, group, fixed selling rate and debit owner. The fixture codec uses only
Python's standard library, validates the handshake and frame bounds, negotiates
no compression, and contacts no external WebSocket service.

Four additional cancellation cases cover both node directions with a normal
close or abrupt socket disconnect. Each must stop physical upstream generation,
release the shared slot, and let the peer recover without replay or a successful
terminal on the cancelled client. Only the current turn's observed partial usage
(fixture input 3 / output 2) may settle once; earlier turns and invented final
usage must not be billed. Failure evidence retains the physical-release boundary
separately from settlement. The fixed `.5`/`.6` pair fails this check because its
ordinary, non-smart passthrough omits unfinished-turn usage; do not relax the
assertion or use that pair for a mixed-traffic rollout.

`websocket_overlap.status=compatible` covers only
`native_responses_websocket_capacity_continuation_and_settlement` for the exact
binary pair. `mixed_version_isolation_verified` stays **false**: WebSocket
unknown-usage cancellation, regional ownership, background jobs, other non-text
capabilities and supplier cache lifetime remain separate observations.
The normal drained upgrade,
rollback, re-upgrade and captured-ID cleanup still run. This check does not
authorize mixed-version production traffic or change the production rollout.

### Explicit two-turn tool verification

`local-tool-smoke` verifies the first approved non-text scenario: Responses
function calling. It reads `{"api_key":"TOKEN"}` from protected stdin, not a
command-line argument. With the repository's existing Python environment:

```powershell
python -B .github/release-tools/release_matrix.py local-tool-smoke `
  --base-url https://HOST/v1 --model MODEL --output RESULT_PATH
```

An optional `--proxy http://HOST:PORT` selects an explicit HTTP CONNECT proxy.
Without it, system proxy discovery is disabled. The probe identifies itself as
`RanSub2API-Tool-Verification/1.0`. It makes at most two application requests,
with no automatic retries; each may incur normal usage charges. The fixed local
tool only adds 7 and 11. No shell, remote execution, files or external side effects
are exposed to the model.

Turn one must produce one complete `add_numbers` call and matching streamed
arguments. Turn two returns the result using the same model/session, retains the
required history only in memory, and must end with one complete response containing
`18`. A text-only answer, missing arguments, failed/incomplete or duplicate terminal,
extra tool call or incorrect final answer fails verification. Results contain
request IDs, event types and only upstream-reported usage counters, never the API
key, encrypted history or full transcripts. HTTP failures retain the status and
server identity without storing private response bodies. Correlate each returned
request ID with exactly one usage record and the original upstream account before
claiming affinity and accounting acceptance. Inspect a failed result before any
new paid invocation.

This is an explicit business-path check, not an automatic monitor or a quality,
pricing, intelligence, cache-hit or reliability guarantee. It does not promote
text-probe evidence into tool eligibility or alter monitoring/scheduling policy.
