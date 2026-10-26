# Issue18 recovery checkpoint

Branch: recovery/issue18-admin-cli. Base: origin/main e70a268.
No available Issue18 remote/local WIP or open PR existed at recovery inspection.
Dedicated worktree; default worktrees unchanged.

## Implemented first integrated slice (UNVALIDATED)
- cmd/nmcp-admin routes non-migration requests to internal/admin.
- typed config show/set: closed allowlist, nullable retention, zero semantics,
  integer bounds, IANA timezone validation; transactional maintenance fence/audit.
- profile create strict JSON + existing ValidateDraft; activate/retire with
  existing DB certification/lifecycle guards and same key advisory lock,
  monotonic activation conflict check; NO implicit regeneration.
- existing standalone AdminRetryFailed now wraps exported AdminRetryFailedTx;
  intended batch use commits effect/audit/checkpoint in ONE transaction.
- shared safe diagnostics do not stringify SQL errors or raw argv.

## Validation actually attempted
No Go executable on PATH or in permitted repository worktrees. Official pinned
Go1.27.1 acquisition/extraction attempted under workspace .local-tools; extraction
failed with Function not implemented throughout files. No alternate path retry,
sandbox escalation, or inaccessible peer clone attempted. Local compile, gofmt,
unit/integration/race/vet NOT RUN. This checkpoint is not acceptance evidence.
No CI started before checkpoint push. Normal CI PG17 is formal DB authority;
PG18 supplementary only. No old Ubuntu self-verifier changes.

## Remaining acceptance (not implemented yet)
- durable regenerate/retry batches: persisted selection/high-water, Media-ID
  order, profile pin, atomic receipts/effects/checkpoint, safe resume.
- wire Checker/Repairer dry-run and finite lifecycle cleanup adapters.
- maintenance status/resume only after successful fresh full checks.
- complete CLI flags/validation/exit-code coverage and safe confirmation.
- real DB/HTTP/CLI tests: null/0/TZ, activation/no backfill, batch crash/resume,
  concurrency and duplicate prevention, target success immutability, audit.
- formatting/compile/full existing regression gates; draft PR and test mapping.
- backup knobs included in config; backup implementation is Issue19, must fail
  explicitly (especially cleanup all/backups), never success-stub.

## Next bounded slice
Persist batch membership and per-item result receipts in a forward migration;
implement regenerate/retry transaction boundaries and CLI entry points. Push
another coherent checkpoint before operational adapters/tests. No merge or
APPROVE authorization; independent match-head review and distinct main gates
required after future merge by reviewer.

## Integrated successor checkpoint
- Added 0020_admin_batch_receipts.sql exact durable membership + immutable
  completed receipts. Begin freezes eligible snapshot, Media-ID order and
  pinned profile ID; pages commit effects, audit, receipts and checkpoint in
  one transaction. UUID-low insert after begin is excluded. Same-job retry uses
  AdminRetryFailedTx, preserves succeeded targets/attempt history and raises max.
- CLI now wires batch flags/dryrun/confirmation/resume and all operational
  command forms. Unsupported Issue19 backup/all cleanup returns failure BEFORE
  touching any cleanup; no success stubs.
- Checker/Repairer reused; repair remains in maintenance. Leader decision:
  existing preflight maintenance requirement MUST remain; normal-mode repair
  dryrun returns conflict/no writes, maintenance dryrun uses existing preflight.
  No 0021 preview authorization relaxation is present.
- Finite rendition Sweep adapter and bounded100 media cleanup use existing
  lifecycle locking/rechecks/storage journals; response explicitly more_possible.
- Maintenance resume runs fresh full check in maintenance; only sealed zero-
  finding, all-source-complete report permits explicit resume, with audit.
- Fixed independent review MED findings: no raw flag errors (io.Discard), human
  named/pretty JSON dereferences retention pointers as null/0/positive. Added
  causal regression tests, typed validation/confirmation, PG17 CLI/audit,
  activation/no-backfill/stale versions, frozen batch/pin/page commit rejection,
  lock conflict/audit, normal-mode repair dryrun no-write tests.
- Existing CI retains all steps; early explicit administrative CLI PG17 evidence
  added after existing role setup; prints gofmt diff so unavailable local gofmt
  can be corrected from authoritative runner output. No verifier changes.

Tests at successor creation: git diff --check PASS only. Local Go remains
unavailable; new tests/compile/format UNVALIDATED until normal CI runs. First
356cb25 CI/Infra still running at last observation (native builds, no results).
Next: inspect successor early PG17 compile/runtime/format evidence, fix actual
failures, add retry crash/max-attempts/success-target tests and operational
maintenance/cleanup/HTTP integration coverage; push normal fixups, open draft PR.

## Canonical formatting and expanded regression slice
Normal Core CI39523633637 exact2bff0a8 FAILED at gofmt-d exit1 BEFORE Go tests.
No compiler or PostgreSQL runtime results were produced; subsequent steps skipped
are NOT passes. Applied runner-produced formatting patch after git apply--check
against the same frozen source. Formatting gate remains mandatory/unchanged.
Added tests for retry commit rejection/no allowance gap, restarted receipt replay,
same-job/max-attempts/history, byte-identical successful target, real CLI->HTTP
job visibility/no duplicates, maintenance fresh clean check versus missing-file
rejection, finite media cleanup/dryrun and unsupported backup cleanup. These new
tests are not yet executed pending successor normal CI.

## Review durability fixes / formal early results
- Formal PG17 Core39523920613 exactdeb2501 reached compile/runtime: frozen
  membership/pin/crash, lock+audit, normal repair preview rejection, maintenance
  fresh checks and finite media cleanup passed; CLI integration failed fixture
  schema routing, retry fixture failed forbidden max decrease. Neither full
  Core nor skipped downstream steps passed. eb24935 fixed exhaustion fixture;
  Core39523992631 then only the2 CLI schema-routing tests failed.
- Corrected CLI test URLs explicitly with search_path (pgx Config.ConnString
  preserves original string, not RuntimeParams edits), including prior migrate
  CLI test helper. No production error details exposed to make tests pass.
- Fixed new independent MED1: rejected batch COMMIT returns pre-page committed
  counters/checkpoint/status; uncertain network COMMIT returns unknown state,
  never speculative terminal success. Added returned-state assertion.
- Fixed MED2 design:0020 also journals bounded cleanup run membership and exact
  affected-ID authorization BEFORE lifecycle effects. Each item receipt+result
  audit commits together, and later invocations automatically recover unfinished
  run from exact native cleanup/purge journals after result-audit failure/crash.
  Added both media/rendition audit rejection/recovery causal tests. Repair keeps
  durable report->run/manifest/result linkage; repeated same-report execution
  adopts existing plan/results rather than losing effect identity.
- Resume now holds REAL maintenance row FOR UPDATE throughout fresh checker
  scan and audit/cutover commit; direct Repairer uses FOR SHARE and cannot cross
  it even without the CLI session lock. Added actual direct-repair filesystem
  rename barrier vs resume conflict and maintenance preview no-write test.
All current new fixes/tests await successor normal CI. Mandatory formatting guard
and prior full gates remain unchanged. No local Go workaround/bypass used.

## Engineer ownership resumed
Exact ca4f9ec Core39524524362 failed at early gofmt, before tests (two test
spacing differences); Infra39524524406 succeeded. Applied only the exact
runner-produced formatting changes. No runtime/full-Core acceptance inferred.
Next formal successor must execute PG17 early tests and every existing gate.

## Formal 1a79d2e runtime result and repair compatibility fix
Core40248198934 reached real PG17 tests: config/profile, frozen batches, retry
receipts, HTTP visibility, fresh maintenance checks and both cleanup recovery
cases passed. Direct repair causal test failed BEFORE filesystem barrier:
0019 repair admission explicitly supported only migration17/18/19, while new
reports correctly record migration20. Migration0020 now explicitly extends the
read-only admission function and immutable plan-seal version check to20, keeping
all source completeness/seal/schema-name/checksum/classifier guards unchanged.
No change to maintenance authorization or evidence format. This is a production
compatibility defect, not a fixture error; tests must run again on successor.
Draft PR45 already exists with Closes18, not ready until full exact-head gates.

## First resumed formal PG17 results and schema compatibility fix
Core40248198934 exact1a79d2e passed formatting and ran early tests. All admin
integration tests except direct repair vs resume passed, including both repaired
CLI fixtures and media/rendition durable audit recovery. The remaining test
failed before preview:0019's explicit supported migration allowlists rejected
reports created after0020.0021 extends only those version lists (20 and21),
keeping the exact sealed/source/checksum admission predicate, definer owner,
closed search path, ACL and maintenance authorization unchanged. Existing causal
preview/plan/execute/fence test must now run on latest schema; downstream full
Core gates remain unaccepted until a successful successor.

## Isolated single-writer reconciliation
Leader authorized a new UUID worktree/branch based on dfb4693 after concurrent
same-role writers were detected on the old shared worktree. Old path and its
uncommitted patch are preserved unchanged. This ordinary successor removes the
duplicate0021 report_compatibility file, retaining schema_compatibility with its
explicit original definer owner/search_path. Restores0020 byte-for-byte from
ca4f (avoid rewriting an already applied migration). Only forward0021 admits
20/21 reports; negative report authority cases and latest report→plan seal
assertions imported from the saved patch. Runtime still unvalidated. Three
reviewer durability MEDs and Ubuntu verifier latest-version pin remain OPEN.

## Isolated durability fixes (awaiting formal runtime)
- M1 final cleanup receipt/audit and run finished_at now share one transaction;
  selection converges historical exhausted runs before selecting new due work.
- M2 canceled bound purge authorization is completed as skipped, not reassigned;
  re-deleted Media requires a new finite run authorization. Never execute an
  enqueued job differing from the immutable bound ID; binding requires one row.
- M3 rejected Begin COMMIT clears token/membership, transport ambiguity reports
  unknown identity without speculative counts/status. Added deferred rejection
  plus CLI output test and unknown-result unit test.
- Added causal old exhausted run→new due work/final-receipt rejection/recovery,
  and real restore→redelete→new authorization→late audit rejection/recovery tests.
- Ubuntu successful-current assertions now require exact21 plus retained exact
  19/20 evidence; historical18/failure expectations and all phases unchanged.
Local diff-check and shell syntax PASS; no local Go runtime claims. Formal
successor must execute every test and retained mandatory gate before acceptance.

## Finite verifier and concurrency coverage
Added unprivileged finite tests executing actual current-schema helper SQL on
isolated relational fixtures: exact19+20+21 accepted; wrong current name/hash,
dirty20/21, altered historical identity and future22 rejected. This is not a
substitute for privileged PostgreSQL Ubuntu phases. Added the three new admin
journal tables to existing exact protected relation/column ACL inventory.
Added simultaneous profile activators with greatest-successful-version and
no implicit jobs assertions. Local Python verifier suite4tests PASS, shell
syntax and diff-check PASS; all Go/full gates still require formal final head.

## Expanded six-finding review acceptance
Read full prior signed reviewer six-finding comment as leader-required scope.
Additional purge interrupted-lease recovery and native-ineligible rendition
progress are acceptance blockers, not covered by the earlier three MED closure.
This checkpoint adds native-equivalent rendition candidate predicates and safe
terminal skipped receipts when a frozen candidate becomes non-actionable;
native revalidation still owns unlink. Real PostgreSQL COMMIT is followed by a
lost-reply fault seam for Begin and Page, asserting unknown responses then exact
restart/receipt replay and no duplicate retry allowance. Removed uncalled Sweep
adapter. Additional exact-bound purge recovery and reverse maintenance fence
causal tests remain in progress. No full-gate or reviewer approval inferred.
