# Direct Ubuntu acceptance for #16

The disposable GitHub `ubuntu-24.04` VM is the isolation boundary. This is not a
supported command on production hosts. `Ubuntu acceptance` requires both the
13-phase compatibility lane and **every** independent production matrix lane;
`always()` aggregates failure/cancellation/skips as red. Matrix fail-fast is off.
The existing Core CI and Infra validation remain required separately. In particular,
Core CI runs real PostgreSQL reconciliation/check/repair, lifecycle history,
crash recovery, race and storage tests; Ubuntu compatibility is not a substitute.

## Preserved acceptance

* Exact `github.sha` Git archive (PR merge SHA for PR events; branch SHA for manual
  and main events), root-owned snapshot in a fresh root-created `/run` directory.
  Builds current and pinned v18 (`172583179d860619ae2c79505971e10ae7f394fc`)
  admin with local Go 1.27.1. Logs SHA, toolchain, phase start and phase success.
* Stateful order: fresh-current → current-rerun → hostile-libpq →
  stale-compatibility → migration-failure → signal-int → signal-term →
  sigkill-recovery → graph-verifier-failure → upgrade-v18-v19 →
  unfinished-v18-rejection → completed-v18-rejection → writer-drain.
  Assertion failure stops this dependent state chain; it cannot create fake
  downstream passes. Other production lanes continue on separate VMs.
* Each boundary executes independent `infra/validation/verify-runtime-roles.sql`
  against real PostgreSQL, including NOLOGIN/noninheritance/runtime permissions.
  Existing phase SQL checks verify migration checksums, historical rejection,
  failed-migration hardening, writer drain and signal propagation.
* SIGKILL is observed outside a separate systemd cgroup. Wrapper/admin/setsid
  descendant must belong to that cgroup before kill. All must disappear and
  cgroup must empty. Real PG compatibility must persist after forced death;
  current production wrapper recovers and independent SQL proves hardening.
* Separate matrix entries preserve original shellcheck, production static asset
  checks, real repair bind-mount refusal, config/firewall counterexamples,
  trusted input capture, systemd EnvironmentFile, Nginx HTTP/TLS, read-only
  namespace, actual stopped-PG recovery, whole-service failure, migration lock
  races, release/rollback failure transactions and immutable release staging.
* Runner `finally` and workflow `always()` stop the crash workload. Each matrix
  job retains original always-cleanup steps. VM disposal is the final isolation
  boundary on cancellation; cleanup failure is not converted into acceptance.

## Intentionally not acceptance

Old containment/client/supervisor/stage lifecycle, fault labels, closed diagnostic
schemas, source-shape/hash mirrors, intent publication and their synthetic fault
injections test the retired harness, not #16 or production. They are not rebuilt
or invoked by direct acceptance. Old files remain for historical analysis;
`verify-ubuntu-deployment --production-only` keeps production checks but bypasses
legacy workflow/source-shape and diagnostic mutations. The unflagged historical
validator still exists, but is not a claim about the replacement workflow.
No production wrapper, role SQL or installed service security policy is relaxed.
The phase script only adds an explicit cgroup-crash fixture selector for the new
independent observer; existing historical selector remains unused.
