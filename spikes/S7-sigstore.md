# S7: sigstore-go in the daemon

**Verdict: go cosign keyless.** sigstore-go verified a real GitHub Actions keyless bundle with no network, against the embedded trusted root and an identity. It refused a tampered blob, a wrong identity and a missing bundle. The same code accepts cosign key-pair bundles for dev builds. The cost is size: the daemon grows from 8.2 to 20.2 MB on linux/amd64 (+4.2 MB gzipped). minisign would add 70 KB, but it needs a long-lived secret in CI and loses the workflow identity and the transparency log. I think 12 MB is a fair price for a binary downloaded once per version. If it is not, minisign drops in with no other spec change.

## Module

- `github.com/sigstore/sigstore-go` v1.3.0 (2026-07-30), the upstream repo under the sigstore org, not a fork. Apache-2.0.
- v1.0.0 shipped 2025-05-12. The README calls it stable and ready for production, and it passes the sigstore-conformance suite. Three minor releases since (1.1 to 1.3); nothing in this spike depended on a post-1.0 API change.
- It pins `github.com/sigstore/sigstore` v1.10.8. The latest, v1.11.0, needs Go 1.27, so `go mod tidy` must not float it.

## Size (`CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"`, Go 1.26.5)

| target | daemon now | + verifier | + verifier + TUF | + minisign |
|---|---|---|---|---|
| linux/amd64 | 8.21 MB | +11.71 MB (gz +4.05) | +12.01 MB (gz +4.15) | +0.07 MB |
| darwin/arm64 | 7.76 MB | +11.55 MB (gz +3.88) | +11.86 MB (gz +3.97) | +0.02 MB |
| windows/amd64 | 8.54 MB | +12.03 MB (gz +4.14) | +12.34 MB (gz +4.24) | +0.07 MB |

The weight comes from transitive packages, not from sigstore-go itself: 545 packages in all, among them grpc (65), go-openapi (47) for the Rekor client (19), protobuf (38), otel (20), certificate-transparency-go (12) and klog. TUF refresh costs only 0.3 MB more. `s7-sigstore/size.sh` rebuilds HEAD's daemon with each hook. It forces `GOTOOLCHAIN=local`, because a first run silently switched one variant to Go 1.27 and inflated minisign to +0.55 MB.

## Offline proof

- A workflow on a throwaway branch `spike/s7-sign` ran `cosign sign-blob --yes --bundle` (cosign v3.0.6, `id-token: write`) and uploaded blob and bundle as a 1-day artifact. The bundle is v0.3: Fulcio certificate, one Rekor v1 entry and one TSA timestamp. Signing writes that entry to the public Rekor log, as any keyless signature does. Nothing else left the repo. The branch is deleted.
- `s7verify` ran in `docker run --network none alpine` (a `wget` to the TUF CDN fails with "bad address" in the same container):

| case | result |
|---|---|
| blob + bundle, SAN = the spike workflow on its branch | `OK keyless https://github.com/babs/grill-room/.github/workflows/s7-sign.yml@refs/heads/spike/s7-sign` |
| tampered blob | `REFUSED: ... invalid signature when validating ASN.1 encoded signature` |
| release identity (`release.yml@refs/tags/vX.Y.Z`) | `REFUSED: ... expected SAN value to match regex ... got ".../s7-sign.yml@refs/heads/spike/s7-sign"` |
| SAN of another repo | `REFUSED: ... expected SAN value to match regex` |
| missing bundle | `REFUSED: load bundle: ... no such file or directory` |

The production identity is issuer `https://token.actions.githubusercontent.com` and SAN `^https://github\.com/babs/grill-room/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`. `verifier_test.go` replays every case from `testdata/` and goes red when the identity check is relaxed to `.*` or the key check ignores its error.

## Trusted root and TUF

- `tuf.New` bootstraps from a TUF `root.json` embedded in sigstore-go, then fetches `trusted_root.json` from `tuf-repo-cdn.sigstore.dev`. `root.NewLiveTrustedRoot` refetches every 24 h in a goroutine.
- Cadence seen today: `timestamp.json` expires after 7 days (version 805), `root.json` on 2026-11-20. `trusted_root.json` changed 7 times since 2023-02. The last change, 2025-09-22, added the first Rekor v2 shard (`log2025-1`). The one before added the TSA.
- `trusted_root.json` has no expiry of its own. Keys carry validity windows, checked against the signing time. A 2025-01 root (no TSA, no Rekor v2) still verifies today's bundle offline.
- A stale root fails only on a key it lacks. With the Rekor v1 key removed: `not enough verified log entries from transparency log: 0 < 1`. With the current Fulcio CA removed: `leaf certificate verification failed`. This is the real risk: once cosign signs into Rekor v2 or a new shard, an old embedded root refuses new releases until it is refreshed.
- Offline TUF: once `timestamp.json` is past its 7 days, `tuf.New` must refresh and errors out. `NewLiveTrustedRoot` fails if its first fetch fails, logs refresh errors through the stdlib `log`, and caches under `$HOME/.sigstore/tuf` by default.
- Plan for phase 3: try TUF with the cache in the supervisor's state dir, write the fetched `trusted_root.json` there, fall back to the last good copy, then to the embedded one. Show which root was used on the admin page.

## Dev keys

Yes. `VerifyKey` takes a cosign public key, checks the signature at the current time and needs no transparency log. cosign v3 signs offline only with `sign-blob --key cosign.key --use-signing-config=false --tlog-upload=false --bundle` (a deprecated flag; the alternative is a signing config with no Rekor URL). The supervisor tries the keyless identity first, then each dev key: the two need different policies, so one call cannot cover both. Tested: right key passes; tampered blob, other key and a keyless bundle are refused.

## Files

- `s7-sigstore/verifier/`: `VerifyKeyless`, `VerifyKey`, `FetchRoot`, the embedded root and the tests.
- `s7-sigstore/main.go`: the `s7verify` CLI. `s7-sigstore/size.sh`: the size table.
