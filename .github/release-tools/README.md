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


## Custom stable channel without CI

The user-owned public channel is `a5116107/ransub2api`. Its `production` branch retains custom history. `sync-upstream.yml` tracks stable `ranxi2001/sub2api` releases every six hours. `upstream/stable` mirrors the pinned application sources with the owner's `.github/workflows` directory retained. Each tracking commit includes the raw upstream commit as a parent and in `Upstream-Commit`; all non-workflow files must match it exactly. This avoids requiring workflow-write credentials in Actions or importing upstream CI. Never use this tracking branch as a production artifact. The unrelated `a5116107/bps2api` fork is unchanged.

A one-time local `python -B .github/release-tools/sync_upstream.py --bootstrap-tracking` transitions an existing raw tracking branch to the owner workflow policy. It uses existing owner credentials, a normal fast-forward push and a `[skip ci]` commit, without storing a long-lived token in GitHub. Subsequent scheduled pushes use the ephemeral `github.token`; the script verifies unchanged workflow files before pushing. Upstream history rewrites stop synchronization. Git failures show sanitized stderr.

The helper preserves an unmerged `integration/upstream-stable` candidate. If that candidate is already merged, the next release is proposed from the new tracking tip instead of creating an empty PR from the old candidate. Review semantic conflicts and run local regression, migration and rollback checks before promotion. Scheduled synchronization does not merge, compile, publish or deploy; publishing remains a separate local stage. All imported workflows retain manual triggers; build/test CI is not run.

Use `X.Y.Z-ransub.N` for stable custom revisions. Tags alone do not start a release: publishing is explicit. Existing build/test workflows remain disabled. Build from a committed clean source tree, then publish its exact tag and archives:

```powershell
python .github/release-tools/release_matrix.py local-build --simple --output H:/Root/DevCache/releases/ransub2api
git tag -a vX.Y.Z-ransub.N -m "Custom stable release"
git push origin vX.Y.Z-ransub.N
python .github/release-tools/release_matrix.py local-publish --input H:/Root/DevCache/releases/ransub2api --notes NOTES_PATH
```

`--simple` builds a real Linux amd64 archive including the embedded frontend; it is not the old image-only simple release. Other targets remain unavailable until built and verified. Publication first creates a draft and compares GitHub-calculated asset digests before making it latest. Failures leave the draft unpublished. No GitHub credential is included in the package or required by public update downloads.

To publish the corresponding GHCR runtime image, manually dispatch the existing Release workflow with `tag=vX.Y.Z-ransub.N` and `prebuilt_only=true`. This path verifies the downloaded archive against its source commit and packages it without compiling or running CI. Confirm public image pull access before pinning production to its digest. Container update binaries must persist across recreation; keep the previous image and database/data backups until deployment acceptance is complete.

## Local upgrade, rollback and shared-state rehearsal

Use the existing CLI with a saved, verified Linux amd64 custom release and a locally built candidate:

```powershell
python .github/release-tools/release_matrix.py local-rehearse `
  --baseline H:/Root/DevCache/releases/ransub2api/2.21.0-ransub.3 `
  --candidate CANDIDATE_BINARY --candidate-sha CANDIDATE_SHA `
  --scratch H:/Root/DevCache/tmp/sub2api-v2211-verify `
  --output H:/Root/DevCache/evidence/sub2api-v2211-integration/mixed-shared-state.json
```

The candidate must be Linux amd64 with its full source SHA in `-version`. The baseline manifest/archive hash and both binary identities are checked before creating the fixture. The command requires a local Docker daemon and already available `python:3.11-slim`, `postgres:17-alpine` and `redis:7-alpine` images; it never pulls images or accesses production. Override the image options only with compatible locally installed images. The runtime image needs Python 3.11 or later for the standard-library loopback HTTP client and `/proc/1/exe` digest check.

An internal Docker network and tmpfs-backed data isolate the fixture; no port is published. Synthetic admin/account data completes normal fixture onboarding, without changing the production guard or user acknowledgements. Stages are baseline → stopped baseline → candidate → stopped candidate → old binary rollback → stopped rollback → candidate again. Each stage verifies the actual running binary hash, health and authenticated API access. Checks cover unchanged old migration checksums, account credentials/pricing and paused performance-monitor state, a legacy-shaped save retaining the candidate's TPS target, an ordinary account edit after rollback, and idempotent re-upgrade.

After the serial stages, both binaries run concurrently against the same PostgreSQL and Redis. A non-exclusive standard group and an assigned synthetic API Key exercise the normal group-assignment guard. Three rounds concurrently dispatch legacy Key-name saves and candidate context-policy saves, then verify protected account/monitor fields and Key limits. With process-local L1 disabled only in this fixture, alternating read-only `/v1/usage` requests verify versioned Redis snapshots and the candidate's current policy. The checks require unchanged migration history and zero usage rows; they send no generation requests. The `mixed_shared_state` result records this narrower acceptance separately from `mixed_version_isolation_verified`, which remains false.

Both nodes then restart with L1 enabled and a 600-second TTL. After warming them, eviction of only the fixture's L2 entry must leave both nodes able to authenticate without repopulating Redis, proving process-local reuse. Disabling the Key through the old node must make both reject it with HTTP 401; restoring it through the candidate must make both return HTTP 200 again. Each transition has a three-second observation bound, well before cache expiry. The candidate policy and protected state must survive recovery. These results are recorded separately in `mixed_l1_invalidation`.

The L1 phase also removes subscription permissions only on its disposable Redis, closes pubsub connections, and saves a revocation while `PUBSUB NUMSUB` proves there are zero listeners. Permissions are restored in `finally`, including failed saves. Both subscribers must recover within 12 seconds and reject the revoked Key before restoration. The test repeats with each node writing, preserves partial results on failure, and leaves migration/account/policy state unchanged. This specifically checks missed-notification recovery; it does not assert immediate revocation during an ongoing partition.

The default `--l1-peer baseline` checks the old and new binaries together. For recovery after a fully drained upgrade, use `--l1-peer candidate`: only the final L1 pair uses two candidate processes; all earlier serial upgrade/rollback and shared-state stages still exercise both versions. `mixed_l1_invalidation.peer_kind` distinguishes the profiles. A passing candidate pair does not replace a failed baseline-compatibility result or permit an incompatible old node to continue serving strict products. The current subscriber surfaces Redis protocol errors to the existing service retry loop, reports acknowledged reconnects to invalidate stale L1 epochs, checks an idle socket after three seconds and bounds a missing heartbeat response to five more seconds. Cancellation closes pending reads. These are local recovery bounds, not a production revocation SLA.

This proves a serial, drained rollout, shared configuration/authentication compatibility and cross-node L1 Key revocation/recovery on newly initialized disposable data. It does not prove strict scheduling/admission enforcement by old nodes, all cache/network failure modes, a restored production-data upgrade, upstream model quality or cache lifetime. Retain those acceptance items separately; passing shared-state probes do not authorize mixed-version production routing. Resource IDs and outcomes are written to the requested result; cleanup verifies ownership before removing only its containers/network and extracted baseline. Unknown ownership, Docker errors and failed cleanup remain failures with retained evidence. The caller owns the candidate binary and removes it after verification; release archives are retained.
