# Releasing resticscope

This is the maintainer runbook for developing, validating, publishing, and
verifying a `resticscope` release.

The short version is:

```text
pull request or push to main
    -> Linux and macOS CI
    -> golangci-lint, govulncheck
    -> GoReleaser snapshot, without publishing

vX.Y.Z tag
    -> Linux and macOS release verification, including govulncheck
    -> GoReleaser publishes the GitHub Release
    -> GitHub attests every archive in checksums.txt
```

GoReleaser normally runs on GitHub, not on the maintainer's machine.

## Workflows and configuration

The release system has five relevant files:

- `.github/workflows/ci.yml` validates pull requests and pushes to `main`.
- `.github/workflows/release.yml` validates and publishes version tags.
- `.goreleaser.yaml` defines release targets, archive contents, checksums, and
  version metadata.
- `.github/dependabot.yml` proposes weekly updates to pinned GitHub Actions and
  Go modules, grouped into one pull request per ecosystem.
- `scripts/third-party-notices.sh` regenerates `THIRD_PARTY_NOTICES.md`.

GitHub Actions are pinned to full commit SHAs. The comments beside those SHAs,
such as `# v7.0.1`, let Dependabot identify and update them.

## Normal development

For an ordinary change, run the relevant tests locally and push the change.
Installing or running GoReleaser locally is not required.

A reasonable local check is:

```console
gofmt -w path/to/changed.go
go test ./...
go vet ./...
golangci-lint run ./...
```

Use the race detector for concurrency-sensitive changes or when a full local
check is useful:

```console
go test -race ./...
```

`golangci-lint` is pinned to the same version in `.github/workflows/ci.yml`
that you should install locally, so CI and local results stay identical:

```console
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
```

Bump the workflow and the local install together.

CI is the authoritative cross-platform check. Its test matrix runs on Linux
and macOS and performs:

1. `go mod verify`
2. `go mod tidy`, followed by a clean `go.mod` and `go.sum` diff check
3. a `gofmt -l` cleanliness check
4. `go vet ./...`
5. `go test -race ./...`
6. `go build ./cmd/resticscope`

In parallel, CI also runs `golangci-lint`, `govulncheck`, and:

```console
goreleaser release --snapshot --clean
```

The snapshot builds all configured targets and packages the release without
publishing anything. It catches GoReleaser syntax errors, cross-compilation
problems, missing archive files, invalid templates, and similar packaging
regressions before release day.

The packaging job intentionally runs in parallel with the test matrix. This
provides packaging feedback quickly; the overall CI workflow still fails if
any job fails.

## Dependencies and the Go version

`go.mod` pins an exact patch release, for example `go 1.26.8`, rather than a
minor version. Both CI and the release workflow install the Go version from
`go.mod`, so that line decides which toolchain builds the published binaries.
Raising it is how a standard-library fix reaches a release.

That matters here: resticscope extracts snapshot contents through `os.Root`,
so `os` package advisories are directly relevant to it. `govulncheck` runs in
CI and again at tag time precisely to catch this. When it reports a standard
library vulnerability, raise the Go version:

```console
go get go@1.26.8
go mod tidy
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

After changing dependencies, refresh the notices that ship in every archive:

```console
./scripts/third-party-notices.sh
```

The script is deterministic; if it produces no diff, nothing needs committing.
Dependabot pull requests do not run it and CI does not check the notices, so
run it on a Dependabot branch before merging.

## When to run GoReleaser locally

Local GoReleaser is optional. Consider it when:

- changing `.goreleaser.yaml`;
- changing supported operating systems or architectures;
- changing CGO, build tags, linker flags, archive contents, or asset names;
- diagnosing a GoReleaser failure reported by GitHub Actions;
- inspecting an archive before pushing a packaging change.

Even in those cases, pushing a branch and letting the CI snapshot validate it
is acceptable. A local run merely shortens the feedback loop.

If local validation is needed, use the same version pinned in the workflow:

```console
go install github.com/goreleaser/goreleaser/v2@v2.17.0
goreleaser check
goreleaser release --snapshot --clean
```

GoReleaser has a large build-time dependency graph, so installing it from
source can consume several gigabytes of Go module and build cache. It is not a
tool that every contributor needs to keep installed.

## Version numbers

Releases use annotated tags in the form `vX.Y.Z`, for example `v0.1.2`.

Choose the next version according to the scope of the change:

- patch: compatible fixes and small hardening changes, such as `v0.1.1` to
  `v0.1.2`;
- minor: new compatible functionality, such as `v0.1.2` to `v0.2.0`;
- major: an intentionally incompatible public contract.

The public contract includes the config file schema, the secrets JSON
document, subcommand names and flags, and documented exit codes.

Do not reuse a version number. Do not move a published version tag.

## Pre-release checklist

Before creating a tag:

1. Confirm the intended changes are on `main`.
2. Confirm the worktree is clean.
3. Confirm local `main` matches GitHub `main`.
4. Confirm the latest CI run for that exact commit passed.
5. Confirm `THIRD_PARTY_NOTICES.md` matches the current dependencies.
6. Confirm the proposed tag and release do not already exist.

Start from an up-to-date branch:

```console
git switch main
git pull --ff-only
git status --short --branch
git rev-parse HEAD
```

The status should be clean and should not report that `main` is ahead of or
behind `origin/main`.

Inspect the latest CI runs:

```console
gh run list \
  --repo alexzeitgeist/resticscope \
  --workflow CI \
  --branch main \
  --limit 5
```

The green run's commit must match `git rev-parse HEAD`. A green run for an
older commit is not sufficient.

Check the proposed version, replacing `v0.1.2` with the actual version:

```console
git tag --list v0.1.2
gh release view v0.1.2 --repo alexzeitgeist/resticscope
```

The first command should print nothing. The second should report that the
release was not found.

## Create and push the release tag

Create an annotated tag on the green commit:

```console
git tag -a v0.1.2 -m "resticscope v0.1.2"
```

Verify that it is an annotated tag and resolves to the intended commit:

```console
git cat-file -t v0.1.2
git rev-parse v0.1.2^{}
git show --no-patch --format=fuller v0.1.2
```

`git cat-file` should print `tag`, and the dereferenced SHA should match the
green `main` commit.

Push only the new tag:

```console
git push origin v0.1.2
```

The tag push triggers `.github/workflows/release.yml`.

## What the release workflow does

The release workflow first runs an independent verification matrix on Linux
and macOS. Each job verifies modules, checks `go mod tidy`, runs vet and race
tests, builds the CLI, and runs `govulncheck`.

The publication job declares `needs: verify`, so it cannot begin unless both
platform jobs pass. This protects releases even if a tag is accidentally
pushed for a commit whose ordinary CI did not run or did not finish.

After verification, the publication job:

1. checks out the complete tag history with `fetch-depth: 0`;
2. sets up the Go version declared in `go.mod`;
3. runs GoReleaser v2.17.0 with `release --clean`;
4. creates and publishes the GitHub Release;
5. passes `dist/checksums.txt` to `actions/attest` so every listed archive gets
   signed SLSA provenance.

The workflow starts with `permissions: {}`. The verification jobs receive only
`contents: read`. The publication job receives:

- `contents: write` to create the release and upload assets;
- `id-token: write` to obtain the GitHub OIDC identity used for signing;
- `attestations: write` to publish artifact attestations.

The repository-scoped `GITHUB_TOKEN` publishes the release. A personal access
token is neither needed nor expected.

Release concurrency is grouped by tag, and a running release is not cancelled
if another tag is pushed.

## Watch the release

Find the run started by the tag:

```console
gh run list \
  --repo alexzeitgeist/resticscope \
  --workflow Release \
  --limit 5
```

Then watch it, replacing `RUN_ID` with the run's numeric ID:

```console
gh run watch RUN_ID \
  --repo alexzeitgeist/resticscope \
  --exit-status
```

The expected job order is:

```text
Verify release on macos-latest  --+
                                  +--> Publish release
Verify release on ubuntu-latest -+
```

Do not announce the release until the publication job and the attestation step
both pass.

## Release artifacts

GoReleaser builds with `CGO_ENABLED=0` for:

- Linux AMD64
- Linux ARM64
- macOS AMD64
- macOS ARM64

Windows is intentionally not packaged because extraction is refused there.

For version `0.1.2`, the expected assets are:

```text
resticscope_0.1.2_linux_amd64.tar.gz
resticscope_0.1.2_linux_arm64.tar.gz
resticscope_0.1.2_darwin_amd64.tar.gz
resticscope_0.1.2_darwin_arm64.tar.gz
checksums.txt
```

Each archive must contain:

```text
resticscope
README.md
LICENSE
THIRD_PARTY_NOTICES.md
config.example.toml
config.explained.toml
docs/browsing.md
docs/cli.md
docs/configuration.md
docs/extract.md
docs/secrets.md
docs/themes.md
```

GoReleaser injects the tag version, source commit, and build date into the
binary with linker flags. A released binary should report, for example:

```console
$ resticscope version
resticscope 0.1.2 (commit abcdef0, built 2026-07-25T12:00:00Z)
restic 0.19.1
```

A binary built without those flags falls back to the metadata the Go toolchain
embeds, so `go install` and local builds still identify themselves.

GoReleaser writes the release notes from the subject lines of the commits
since the previous tag, grouped by their `feat:`, `fix:`, and `build:`
prefixes; `docs:` and `test:` commits are left out. Commit bodies do not
appear, so write subjects that read well on their own.

## Verify the published release

First inspect the release metadata:

```console
gh release view v0.1.2 \
  --repo alexzeitgeist/resticscope \
  --json url,isDraft,isPrerelease,isImmutable,publishedAt,tagName,assets
```

The expected state is:

- `isDraft: false`
- `isPrerelease: false`
- `isImmutable: true`
- exactly four archives and `checksums.txt`

Download the public artifacts into a fresh temporary directory:

```console
release_dir="$(mktemp -d)"
gh release download v0.1.2 \
  --repo alexzeitgeist/resticscope \
  --dir "$release_dir"
```

Verify every downloaded archive against the published checksum file:

```console
cd "$release_dir"
sha256sum --ignore-missing -c checksums.txt
```

All four archives must report `OK`.

Inspect and test at least one package:

```console
tar -tzf resticscope_0.1.2_linux_amd64.tar.gz
mkdir linux-amd64
tar -xzf resticscope_0.1.2_linux_amd64.tar.gz -C linux-amd64
./linux-amd64/resticscope version
./linux-amd64/resticscope --help
```

Verify GitHub's signed provenance:

```console
gh attestation verify \
  resticscope_0.1.2_linux_amd64.tar.gz \
  --repo alexzeitgeist/resticscope
```

The verification should identify:

- repository `alexzeitgeist/resticscope`;
- source ref `refs/tags/v0.1.2`;
- `.github/workflows/release.yml` as the signing workflow;
- the exact tagged commit;
- predicate type `https://slsa.dev/provenance/v1`;
- all four release archives as subjects.

## Immutable releases

Enable GitHub release immutability for this repository. Once a release is
published, its tag and assets then cannot be replaced or deleted.

This is a safety feature, not an inconvenience to work around. If a published
release contains a defect:

1. do not move its tag;
2. do not attempt to replace its assets;
3. fix the problem on `main`;
4. let CI pass for the fix;
5. publish the next patch version.

For example, fix a problem in `v0.1.2` by releasing `v0.1.3`.

## Failure handling

### Ordinary CI fails

Do not tag the commit. Fix the failure, push the fix, and wait for a green CI
run on the new commit.

### govulncheck fails

Treat it as a release blocker rather than an inconvenience. A dependency
advisory usually means a module bump; a standard library advisory means
raising the `go` line in `go.mod`. Both paths end with a green CI run on a new
commit, which is then the commit to tag.

### Tag-time verification fails

The publication job will not start. Inspect the failed Linux or macOS job.

- If the failure was transient infrastructure trouble and the source does not
  need to change, rerunning the same workflow is reasonable.
- If source or workflow changes are needed, fix them on `main`, wait for CI,
  and use the next version. Do not silently move the already-pushed tag.

### GoReleaser fails before publication

Inspect the workflow logs and the Releases page for a draft. Do not manually
assemble or upload binaries from a different build environment. Rerun only for
a genuinely transient failure with unchanged source; otherwise fix the issue
and publish the next patch version.

### Attestation fails after publication

Do not describe the release as provenance-verified. Because a published
release is immutable, investigate carefully before retrying anything. If the
attestation cannot be completed safely for the exact published artifacts,
publish a corrected patch release rather than replacing or relabelling assets.

### A published release is wrong

Treat it as permanent. Fix forward with a new version.

## Release completion checklist

A release is complete only when all of the following are true:

- the annotated tag points to the intended green commit;
- tag-time Linux verification passed;
- tag-time macOS verification passed;
- GoReleaser publication passed;
- the GitHub Release is public and immutable;
- all four expected archives and `checksums.txt` are present;
- every archive passes checksum verification;
- the archive contains the binary, README, license, notices, config examples,
  and guides;
- the packaged binary reports the expected version and commit;
- `gh attestation verify` succeeds for a downloaded archive;
- local `main`, GitHub `main`, and the intended source commit are understood
  and no uncommitted release changes were left behind.
