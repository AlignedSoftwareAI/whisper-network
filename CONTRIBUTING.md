# Contributing

Thanks for helping. This document covers the development workflow. The
behaviour of the system is specified in [DESIGN.md](DESIGN.md); read it before
changing anything non-trivial.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).
Report security issues privately, as described in [SECURITY.md](SECURITY.md).

## Development setup

Requirements: the Go version in `go.mod`, `make`, and Docker (for images and
the integration test). For running real transcriptions locally you also need
`whisper-cli` and `ffmpeg`; most tests use fakes and need neither.

```sh
git clone https://github.com/deweysasser/whisper-network
cd whisper-network
make tools     # staticcheck, gocritic, gosec
make hooks     # installs the pre-commit and commit-msg hooks (needs pre-commit)
make           # builds ./whisper-network
make test
```

The project is built from the golang-program template. The build system, CI
workflows, changelog and update-from-template flow are described in
[README-TEMPLATE.md](README-TEMPLATE.md); treat it as the build-system
reference.

## Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):
`feat(server): ...`, `fix(worker): ...`, `docs: ...`, `test: ...`,
`refactor`, `perf`, `build`, `ci`, `chore`. The changelog is generated from
them (`.chglog/`), so the type and a short imperative subject matter.

`make hooks` installs [pre-commit](https://pre-commit.com/) with the repository's
`.pre-commit-config.yaml`: gofmt, `go vet`, gosec, staticcheck, gocritic on commit,
and a conventional-commit check on the commit message. Run the hooks before
pushing; CI runs the same kind of checks.

## Design first

Changes to behaviour (API, scheduling, state machines, sandboxing, retention,
log events, metrics) start in DESIGN.md. Update the relevant section in the same
pull request as the code, or open an issue to discuss first. If the code and the
design disagree, one of them is a bug; say which. New log events and metrics
must be added to the catalogs in section 10.

## Tests

Expectations:

- **Table-driven** tests for anything with more than a couple of cases.
- **The store uses a real SQLite file** in `t.TempDir()`. Do not mock the store.
  Concurrency-sensitive behaviour (claims, leases) is tested with real
  goroutines and the race detector.
- Inject clocks, ID generators and process runners rather than reading globals,
  so tests are deterministic and fast.
- **Log hygiene.** Use the `internal/obs/obstest` helpers to capture log output.
  A test must not leave tokens, passwords, prompt text, transcript text or job
  metadata in logs; there is a golden test over the full job path.
- Authorization changes extend the principal x role x scope x action matrix in
  `internal/auth`.
- Sandbox tests are Linux-only and skip with a reason where the kernel lacks
  support; do not delete a skip to make a run green.
- A bug fix comes with a test that fails without it.

Run:

```sh
make test         # go test ./...
make test-race    # with the race detector (CI runs this)
make cover        # coverage summary
make integration  # end-to-end stack test; needs Docker and takes several minutes
```

### Fuzzing

The parsers of hostile input have fuzz targets: `FuzzWAV` and `FuzzFLAC` in
`internal/media`, and `FuzzParseToken` in `internal/auth`.

```sh
make fuzz                                                       # short pass, as in CI
go test ./internal/media -run='^$' -fuzz=FuzzWAV -fuzztime=10m  # longer, single target
```

Crashers are written to `testdata/fuzz/<Target>/`. Commit the failing input
together with the fix so it runs as a regression test.

## Updating pinned versions

**Models.** Models and the VAD model are pinned by sha256 in the table in
`internal/models/models.go`. To add or bump one, take the sha256 and size from
the upstream file's Git LFS object id (Hugging Face shows both), not from a
download you did not verify. Add a peak RSS estimate and a cost factor (refine
them with `whisper-network bench`), and update the default `--models` and
`--model-factors` only if the default set changes. Mention the change in
DESIGN.md if it affects defaults.

**whisper.cpp.** The version is the `WHISPER_VERSION` build argument in the
`Dockerfile`. To bump it:

1. Change `WHISPER_VERSION` to the new release tag.
2. `make image` and confirm `whisper-cli --version` works in the image.
3. Run `whisper-network bench` and `make integration` with the new image.
4. Check that the model table and the `whisper-cli` flags the worker passes
   still match upstream.

The Dockerfile pins its base images by digest, and `WHISPER_COMMIT` must be the
commit the release tag points to (the build fails if the tag has moved). When
bumping `WHISPER_VERSION`, also update `WHISPER_COMMIT`:
`gh api repos/ggml-org/whisper.cpp/git/ref/tags/<tag>` (dereference annotated tags).

GitHub Actions in `.github/workflows` are pinned to full commit SHAs with a
`# vX.Y.Z` comment, and the git-chglog download in `release.yml` is checked
against a pinned sha256. Workflows default to `contents: read`; only the
release and image-push jobs get write permissions. Dependabot proposes Go
module, GitHub Actions (SHA pins) and Docker base image updates.

## Pull requests

- Keep a pull request to one logical change.
- `gofmt` clean, `go vet` and staticcheck clean, tests passing with `-race`.
- Fill in the pull request template, including documentation and design updates.
- Do not include hostnames, internal addresses, real tokens or recordings in
  commits, tests or issue text. Use example values (`example.com`, `nuc-01`).
