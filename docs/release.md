# Releases

GitHub Actions builds and signs every release. Nobody holds a signing key: cosign signs keyless, with a certificate Fulcio issues to the release workflow's identity, and logs each signature in Rekor.

## Workflows

`.github/workflows/ci.yml` runs on pull requests to `develop` and on pushes to `develop` and `feat/**`:

- `go`: `make lint`, `make test` (`go test -race -cover ./...`), `make build`, and `make release VERSION=v0.0.0`, which cross-compiles the six daemon targets.
- `windows`: `go test ./...` on `windows-latest`. See [Tests skipped on Windows](#tests-skipped-on-windows).
- `stt`: `make stt-test`, with the whisper `small` model at the revision the STT image pins. CI sets `CI`, so the e2e test fails instead of skipping when the model is missing.

`.github/workflows/release.yml` runs on a tag `vX.Y.Z`. Pre-release tags such as `v1.2.3-rc1` do not trigger it: the supervisor would refuse their signature anyway (see the identity below).

| Job | Permissions | Does |
|---|---|---|
| `binaries` | `contents: read` | `make release`: six binaries, version stamped, a `.sha256` per binary, `VERSION` |
| `sign` | `id-token: write` | `cosign sign-blob --bundle` per binary, then `cosign verify-blob` against the identity below |
| `image` | `contents: read`, `packages: write` | the sandbox image for linux/amd64 and linux/arm64, one native runner each, with SBOM and provenance, pushed by digest |
| `image-merge` | `packages: write`, `id-token: write` | the multi-arch index `ghcr.io/babs/grill-room/sandbox:<tag>`, signed with its platform images (`cosign sign --recursive`), then `cosign verify` |
| `release` | `contents: write` | the GitHub release |

The build job never holds the OIDC token, and the signing job never runs the Go build. The `binaries` job sets up Go with no cache, so a branch run cannot plant anything in a signed binary.

Release assets:

- `grill-room-daemon-<os>-<arch>` for linux and darwin, `grill-room-daemon-windows-<arch>.exe`, amd64 and arm64;
- `<binary>.sha256`, in `sha256sum` format;
- `<binary>.sigstore.json`, the cosign bundle (certificate, signature, Rekor entry);
- `VERSION`;
- `sandbox-image.txt`: the image reference pinned by digest, `ghcr.io/babs/grill-room/sandbox:<tag>@sha256:<hex>`. The release notes repeat it.

## Signing identity

Every signature, on a binary or on the image, carries this identity:

| | |
|---|---|
| Issuer | `https://token.actions.githubusercontent.com` |
| SAN | `https://github.com/babs/grill-room/.github/workflows/release.yml@refs/tags/<tag>` |
| SAN, as the supervisor matches it (003-AC2) | `^https://github\.com/babs/grill-room/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$` |

A signature made by another workflow, on a branch, or in a fork has another SAN and fails the check.

## Verifying a release

```sh
tag=v0.0.1
gh release download "$tag" -R babs/grill-room -D "grill-room-$tag"
cd "grill-room-$tag"
for f in grill-room-daemon-*; do
  case $f in *.sha256|*.sigstore.json) continue ;; esac
  sha256sum -c "$f.sha256"
  cosign verify-blob --bundle "$f.sigstore.json" \
    --certificate-identity "https://github.com/babs/grill-room/.github/workflows/release.yml@refs/tags/$tag" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com "$f"
done
cosign verify \
  --certificate-identity "https://github.com/babs/grill-room/.github/workflows/release.yml@refs/tags/$tag" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$(cat sandbox-image.txt)"
```

## Shipping a release in the room server

The room server serves the daemon release in `GRILL_ROOM_DAEMON_DIR` (`/daemon` in the image). Each binary's bundle sits beside it as `/download/<binary>.sigstore.json`, and `GET /api/daemon/version` lists the bundle paths under `bundles`. A release without bundles serves none, and `bundles` stays empty. The server refuses to start on a bundle it cannot read.

The image builds an unsigned release by default. To ship a signed one, hand it to the build as a directory:

```sh
gh release download v0.0.1 -R babs/grill-room -D dist --clobber
make docker-build VERSION=v0.0.1 DAEMON_DIST=dist
```

The build fails when `dist/VERSION` differs from `VERSION`. Put `sandbox-image.txt`'s content in `GRILL_ROOM_SANDBOX_IMAGE` so daemons pull the signed image.

## Dev signing

Local builds can be signed with a cosign key pair, for a supervisor told to trust that key (003-AC4). Signing stays offline: no certificate, no Rekor entry.

```sh
cosign generate-key-pair --output-key-prefix /path/to/dev-cosign   # asks for a password
echo 'COSIGN_KEY=/path/to/dev-cosign.key' >> .env
COSIGN_PASSWORD=... make dev-release VERSION=v0.0.0-dev
cosign verify-blob --key /path/to/dev-cosign.pub --insecure-ignore-tlog=true \
  --bundle dist/grill-room-daemon-linux-amd64.sigstore.json dist/grill-room-daemon-linux-amd64
make docker-build VERSION=v0.0.0-dev DAEMON_DIST=dist
```

Keep the key outside the repository. `.env` is untracked, and `COSIGN_PASSWORD` can live there too. `make dev-release` uses cosign v3's `--use-signing-config=false --tlog-upload=false`; cosign prints a deprecation warning for the second flag (spike S7).

## Making the image public

ghcr.io creates a package private the first time a workflow pushes it, and the REST API cannot change a package's visibility. Once, after the first release: open the package (`https://github.com/users/babs/packages/container/grill-room%2Fsandbox/settings`), "Change visibility", "Public". Later releases keep it public. An anonymous pull confirms it:

```sh
docker logout ghcr.io; docker pull "$(cat sandbox-image.txt)"
```

## Tests skipped on Windows

The `windows` job runs `go test ./...` without `-race`. These checks skip there:

| Test | Why |
|---|---|
| `internal/web` `TestInstallScriptInstallsAndStarts`, `TestInstallScriptRefuses` | `install.sh` targets Linux and macOS and refuses MSYS and MinGW |
| `internal/sandbox` `TestDockerStream` | its fake `docker` is a sh script |
| `internal/sandbox/fakeclaude` `TestFakeClaude` | the fake claude runs only in the Linux sandbox and builds project names from POSIX paths |
| `GRILL_ROOM_TEST_DOCKER` tests (`make test-docker`) | they need Docker; they already skip without that variable on every OS |

Two assertions change on Windows:

- the file-mode checks in `TestAdminAllowlist`, `TestAdminSettings` and `TestUpdateReplacesBinaryAndRestarts`. Windows has no mode bits, so the 0600 these files get on Unix protects nothing there. Phase 4 has to cover token files on Windows with ACLs or the Credential Manager.
- `TestUIIsServed` expects `application/javascript` for `.js`, the type the Windows registry gives. The room server ships on Linux.
