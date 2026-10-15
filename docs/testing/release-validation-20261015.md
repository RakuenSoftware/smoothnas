# Release validation repairs — 2026-10-15

Baseline: `testing-2026.1014.0713-8f28469`. The repairs were built and
installed in the dedicated Debian 13 appliance VM 9410 on `.253`.
This is validation of patched components, not a claim that a new release ISO
has already been built and validated.

| Finding | Repair | Regression coverage |
| --- | --- | --- |
| QA-001: job collection without trailing slash returned 404 | Route both collection forms through the authenticated job handler | Real router and persisted session test |
| QA-002: duplicate NFS export persisted after exportfs failure | Reject duplicate path/client pairs, serialize mutations, restore stored/configured exports on apply failure | Duplicate/concurrent create, failed apply rollback, existing update/delete tests |
| QA-003: export creation raced a nonblocking NFS restart | Start rather than restart a serving server; interactive enable waits for TCP 2049 | Listener readiness/deadline test; immediately mount a newly created real export |
| QA-005: cancellation waited 249 seconds for mount | Carry cancellation through TCP probe and kill the mount helper process group | NFS/SMB forked-helper cancellation tests; real backup API with a deliberately stalled mount helper |
| QA-006: update failure/progress disappeared on revisit | Restore both system and package progress; keep discovery errors separate; resume polling without duplicate timers | Four Chromium cases exercising the real application and API fixtures, including reload |
| QA-008: stopped plugin restarted after reboot | Persist operator stop intent separately from observed runtime state | Lifecycle tests, daemon restart, normal API appliance reboot, explicit start afterward |
| QA-009: bridge reconciliation appended NAT rules indefinitely | Atomically replace only the runtime's postrouting chain, sharing the port-forward mutation lock | Isolated network namespace: ten repeated reconciliations retain two masquerade rules and both DNAT rules |
| QA-010: uninstall left a 397 MiB cached image | Combine inventory rows by immutable image ID, preserve images used by other plugins/containers, remove every alias and verify inventory | Mutable/digest alias tests, stopped shared-plugin protection, runtime conflict/error propagation, real runtime uninstall |

The runtime build pins upstream LXC2Docker commit
`7171568494541a1c9a776670a1623f3db64d7e46` and applies a reviewed downstream
patch. The same build script is used by releases and the new runtime CI job.
CI now also runs for pull requests into `testing`. Image deletion no longer forces removal: container-use
conflicts and backend failures are surfaced rather than hidden by force mode.

## Verification

- `go test ./... -count=1`: passed.
- `go vet ./...`: passed.
- Race tests for API, plugins and backup packages: passed.
- Frontend TypeScript and production build: passed. Local package access lacked
  a GitHub Packages token, so the exact SmoothGUI `v0.6.0` public source tag was
  built locally and used with the other locked dependencies. Repository
  dependency manifests and lockfile were unchanged. CI uses the registry package.
- Chromium: system failure restored across refresh/reload, active system polling
  resumed to failure, package failure restored, active package polling resumed
  to completion.
- Appliance: `/api/jobs?tag=array-create` returned 200; export create returned
  201, immediate NFS mount/read succeeded, duplicate returned 409. Starting
  from a stopped NFS server also passed: create completed in 2.141 seconds
  and mounted immediately afterward.
- Appliance: stalled mount cancellation reached a terminal Cancelled result in
  0.003 seconds; the temporary test service override was removed afterward.
- Appliance: normal reboot returned API service in 23.53 seconds and retained
  the stopped plugin. All 96 cache files and 39 tiered files matched their
  pre-repair SHA-256 checksums after reboot.
- Appliance: the deployed production UI passed all four recovery cases with
  API fixtures, and Updates, Sharing, Backups and Containers loaded against
  the real authenticated API without JavaScript errors.
- Appliance: two plugins shared one cached image. Removing the first retained
  both inventory aliases for the stopped second plugin; removing the last
  reclaimed every alias and the 397 MiB template, leaving zero images.
- Runtime: namespace NAT test passed; the actual appliance's old duplicate
  masquerade rules were reduced to exactly two.

Raw logs and appliance fixtures are retained outside the repository in
`/home/virant/dev/smoothnas-validation-20261014/`. Credentials are excluded
from this report and from commits.

## Limits

The earlier `.254` file-loss report remains unroot-caused; these checks do not
establish that it is fixed. The `.253` host's intermittent storage flush delays
remain an infrastructure observation. ARM hardware and a newly built final ISO
still require release validation after these changes merge.
