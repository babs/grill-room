# S9: one supervisor, two instances, two accounts (003-AC31)

**Verdict: pass.** One supervisor on the dev machine served a room on the local compose instance and a room on the lab instance at once. Each room ran on its own Claude account, the two haiku turns overlapped, and the monitoring recorded both with their accounts. 2 real Claude requests.

Test: `e2e/supervisor/supervisor.spec.js`, run with `make e2e-supervisor`. Run on 2026-10-12.

## Setup

| | Local compose instance | Lab instance |
|---|---|---|
| Server | `https://grill-room.<dev-domain>` | `https://grill-room.<lab-domain>` |
| Daemon release served | `v0.0.4-dev.1`, built by `make dev-release` and signed with a throwaway cosign key pair | `v0.0.3`, the GitHub release, signed keyless by the release workflow |
| How the supervisor trusts it | the dev key, added through the page API (`POST /api/dev-keys`) | the identity check (003-AC2) |
| Room account | `default`: the Claude Code config dir | `token`: a `claude setup-token` token, set as the room's override (`POST /api/instance-account` with `room`) |

The supervisor was the `v0.0.3` release binary for linux/amd64, with a fresh state root and the default global cap of 2. The compose server was rebuilt with `make docker-build VERSION=v0.0.4-dev.1 DAEMON_DIST=<dev release dir>`. A dev pre-release above `v0.0.3` passes the worker floor (workers below `v0.0.3` are refused), so the local worker ran a different version from the lab worker. That is the 003-AC7 case.

I picked the dev key route over the per-instance "accept unsigned" toggle. The dev key keeps the signature check on: the worker binary must still verify, against a key the host added.

## What the test checks

1. Both rooms paired from the supervisor's page, each approved by "Alice" in her room.
2. The page reports the compose instance as `signed with the dev key e2e`, and the lab instance at a plain `vX.Y.Z`.
3. The lab room shows `Alice holds the agent(s).`, and its page text does not contain the machine's hostname.
4. Both rooms receive a question, and both Grill buttons are pressed together. While the turns run, `GET /api/status` shows `concurrency.running` with 2 entries.
5. `GET /api/monitor/turns` holds both turns, `status: ok`, with intervals that overlap:

| Room | Account | Model | Start | Duration | Queue wait |
|---|---|---|---|---|---|
| local | `default` | `claude-haiku-5-5` | 07:04:28.818 | 3066 ms | 0 ms |
| lab | `token` | `claude-haiku-5-5` | 07:04:28.115 | 3959 ms | 0 ms |

6. The proxy log has one `POST /v1/messages` per turn: 2 in all, one through each account's proxy.
7. Cleanup deletes both rooms and Alice's two profiles. After the run, `docker ps` listed no container with the `grill-room-root` label: stopping the supervisor removed its sandboxes.

## Around the run

- The lab was redeployed first with the `v0.0.3` daemon release and its bundles. The SQLite file was backed up beforehand, with the server scaled to zero, and passed `PRAGMA integrity_check`. `/api/daemon/version` advertises `v0.0.3` and six bundles. `make e2e-lab` (1 Claude request) and `make e2e-lab-mail` passed afterwards.
- The long-running single-room daemon that hosted the lab's "Phone test" room was moved to the `v0.0.3` supervisor on the same state root (003-AC10). The supervisor migrated the state and kept the old one as `legacy-backup-<date>`. The room reconnected as host without a new pairing, on account `default`.
- A first attempt failed before any Claude request: a release binary fetched with `gh release download` has no execute bit. The spec now cleans up when the supervisor cannot start.
