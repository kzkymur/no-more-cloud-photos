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
