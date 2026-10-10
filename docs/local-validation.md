# Local validation and CI gates

The repository owns the validation entrypoints. GitHub Actions calls the same
commands; it is not the only place they can run.

## Supported workstation

The one-command dependency installer supports Ubuntu 26.04 amd64 and pins every
explicitly requested distro package plus Go 1.27.1. It intentionally does not invoke
`sudo`, alter Docker group membership, change credentials, or enable a daemon.
An administrator runs:

```sh
sudo scripts/install-local-validation-deps
```

Then log out/in if the administrator separately granted an approved Docker
socket policy, and verify the host before spending time on builds:

```sh
scripts/local-validation-preflight quick
scripts/local-validation-preflight postgres
scripts/local-validation-preflight infra
```

Preflight errors name the missing tool or resource. At least 5 GiB must be free
on the checkout filesystem. Keep `TMPDIR` on that filesystem when `/tmp` is
small. Docker validation requires a reachable Engine and Compose v2; a host
with `no_new_privs`, disabled user namespaces, and no Docker socket cannot run
the Compose lane and is not silently weakened or emulated.

## Commands

```sh
scripts/run-validation quick       # modules, format, vet, binaries, unit/local tests
scripts/run-validation race        # real PostgreSQL + native helpers under Go race
scripts/run-validation postgres    # isolated local cluster, or TEST_DATABASE_URL
scripts/run-validation native      # prepare pinned helpers/fixtures and run native evidence
scripts/run-validation infra       # Compose clean/retained lifecycle and native images
scripts/run-validation full        # quick + postgres + native race/evidence + infra
scripts/run-validation ubuntu      # unprivileged dispatcher; audited commands elevate
scripts/run-validation all         # full plus command-scoped privileged evidence
scripts/run-validation cleanup     # checkout-owned state only
```

`postgres` starts an isolated socket-only cluster beneath `.validation/` when
`TEST_DATABASE_URL` is absent. It chooses server tools using `pg_config`. A
portable installation can instead set `NMCP_POSTGRES_BIN`,
`NMCP_POSTGRES_SHARE`, and (when needed) `NMCP_POSTGRES_LIB`. It creates only
the `nmcp_test` database, stops the server on every exit path, removes its short
socket directory, and retains the cluster for faster reruns. Set
`TEST_DATABASE_URL` to use an already managed disposable database instead.

Native preparation downloads only into `.tools/` and `.native-work/`, verifies
cached downloads before use, rebuilds the pinned helpers, checks their reported
capabilities, and writes `.native-work/validation.env`. Run
`scripts/prepare-native-validation COMMAND [ARG ...]` to execute another command
with that environment, or source the generated file after preparation.

Compose uses checkout-hashed project, volume, and ownership labels. Its verify
command guarantees cleanup, while Docker's image/build cache is retained.
PostgreSQL inputs are never exposed directly from the checkout: the
entrypoint accepts only five repository-owned, regular, non-symlink SQL files
whose content matches the exact `HEAD` blobs. A per-project lock serializes
staging, verification, Docker launch, and reset. The helper pins directories by
file descriptor, rejects symlinked path components, verifies file identities
before and after copying, and publishes only pinned snapshot bytes into an
ownership-labeled, per-project/run Docker volume. Compose consumes that
daemon-owned volume rather than a host path that can change between verification
and bind resolution. This keeps validation working after a fresh checkout under
`umask 0077` without broadening checkout modes. Cleanup first atomically detaches
a registered stage beneath the pinned project directory, then removes only
descriptor-relative entries; it fails closed if a directory identity has
changed and never traverses arbitrary host paths. Unregistered partial runs are
removed by the same supervisor before it releases the project lock. `cleanup`
delegates to that reset and otherwise removes only `.validation/`.

The Ubuntu lane is intentionally separate. Its dispatcher must run as the
unprivileged checkout owner and requires passwordless command-scoped `sudo`,
systemd as PID 1,
PostgreSQL, Nginx, and the other packages installed by the installer. Run it in
a disposable Ubuntu VM or an equivalent dedicated runner—not in a developer
container that merely happens to expose `sudo`.
The dispatcher retains the checkout owner for static and Nginx HTTP fixtures and
elevates only named privileged verifiers. Each run allocates a root-owned mode
`0700` random directory and token-bound release/unit registry. Cleanup validates
that registry, stops and checks only registered run-unique transient units, and
removes only initially absent releases registered by that run. A privileged
two-run counterexample proves that cleaning one run preserves the other's
release and a pre-existing release.

## CI topology and timing baseline

Before the split, successful run `37915296604` took 29m10s end to end. Its
largest serial steps were native still build 5m43s, native video build 8m18s,
unit/integration tests 3m41s, and race tests 7m25s. Compose run `37911551976`
took 17m48s (17m38s in `scripts/validation-env verify`). Privileged Ubuntu run
`37911923533` took 2m39s.

CI runs independent `quick`, `postgres`, and `native-race` lanes. The last lane
enables real PostgreSQL and every native helper during `go test -race ./...`,
then retains the explicit native evidence commands. `full-gate` succeeds only
when all three succeed and is the
single required merge check. Superseded branch runs are cancelled. Setup Go
caches modules and build objects; the native lane caches checksum-verified
source archives using OS, architecture, build-script, and preparation-command
content in the key. Native binaries are deliberately rebuilt: their absolute rpaths and
runner package closure make cross-run binary reuse less trustworthy than source
download reuse. This trades several build minutes for stronger provenance.

`CI` and privileged Ubuntu also run weekly and on manual dispatch. Ubuntu PR
path selection is conservative: every shared `scripts/**` change triggers it,
so migration or privileged validation cannot be skipped by editing a shared
entrypoint. Main-branch pushes always run the privileged workflow. Compare the
first run after a cache-key change (cold) with the next unchanged run (warm) and
record both job durations in the pull request; local hardware and Docker cache
state make a universal wall-clock promise misleading.

Measured cold native-source-cache evidence for this split is CI run
`37924475841`: the cache key was absent, the `native-race` job took 25m34s,
its preparation/evidence step took 24m40s, and the complete gate took 25m42s.
The unchanged-key warm run `37927394504` restored the verified 111 MB source
cache; `native-race` took 20m36s, its preparation/evidence step took 17m46s,
and the complete gate took 20m44s. Binaries were rebuilt in both runs.
