# Rolling out the room server

Two pods, replaced one at a time (spec 017, 017-AC19, 017-AC21). A rollout or the loss of one pod should lose no message: `tools/rollout-watch.sh` checks it from outside, through the ingress.

## TL;DR

| Step | Command | Expect |
|---|---|---|
| 1. Build and push | `make k8s-push VERSION=<tag>` | `.env` lines to paste |
| 2. Start the watch, in a second terminal | `ROLLOUT_WATCH_LINES=600 make -s rollout-watch > rollout.log` | `stream open, posting 600 lines` |
| 3. Apply | `make k8s-apply` | `deployment "grill-room" successfully rolled out` |
| 4. Check the pods | below | 2 pods ready, `grill_room_leader` 1 on exactly one |
| 5. Read the watch | `rollout.log` | `result: PASS`; paste it in the PR |
| 6. Delete one server pod | below | `result: PASS`, the replacement Ready |

Every command runs from the checkout, with `KUBECONFIG` set to the cluster's file and `NS=${K8S_NAMESPACE:-grill-room}`. The watch needs `curl` and `jq`, and reads `GRILL_ROOM_ADMIN_TOKEN` and `K8S_DOMAIN` from `.env` (or `GRILL_ROOM_URL` to target another server).

## First rollout from a release before Phase 3

A release before spec 017 Phase 3 (schema below 47) keeps rooms' live state in its pod's memory: its pod shares nothing with a new one. A rolling update would run the two side by side. Stop it first, then apply:

```sh
kubectl -n "$NS" scale deploy grill-room --replicas=0
make k8s-apply
```

The rooms are down between the two, as in the cutover. Every later rollout is a rolling update, as above.

## The watch

`make rollout-watch` runs `tools/rollout-watch.sh`, which:

1. creates a room with the admin token, invites a participant and joins as them;
2. keeps the room's event stream open as a browser does: on a dropped connection it reconnects after 1 s with `Last-Event-ID` set to the last complete event;
3. posts one line per second, `ROLLOUT_WATCH_LINES` lines (default 300). Pick more seconds than the rollout takes. Ctrl-C stops posting early and still reports, but only when the script runs directly (`tools/rollout-watch.sh > rollout.log`, with `GRILL_ROOM_URL` and `GRILL_ROOM_ADMIN_TOKEN` exported): under `make`, Ctrl-C stops make too;
4. waits up to `ROLLOUT_WATCH_SETTLE` seconds (default 15) for the last lines, deletes the room and prints the report.

The admin token and the participant's cookie reach curl through header files, never its command line.

```text
t+0s room <id>, stream open, posting 25 lines
t+8s post 9 failed: HTTP 000
t+10s stream closed (curl exit 18), reconnect 1 with Last-Event-ID 29
...
lines: 25 attempted, 19 posted, 6 not posted (POST failed; 0 of them delivered anyway)
stream: 4 connection attempts, 3 reconnects with Last-Event-ID
received: 19 of 19 posted, 0 lost, 0 out of order, 0 duplicated
result: PASS
```

That run is one local server stopped for 3 s mid-run, so every line in the gap failed. The event log (with `t+` offsets) goes to stderr, the report to stdout.

- `posted`: the server answered 201. Each must arrive on the stream, once, in order; `lost`, `out of order` or `duplicated` above 0 exits 1, as does a replay from the start after a reconnect (counted as duplicates).
- `not posted`: the POST got no 201. A browser shows the user an error and the line is theirs to send again, so it is no loss. One that arrives anyway (committed, answer lost) counts under `delivered anyway`.

### Failed POSTs during a rollout

A pod stops listening as soon as it gets SIGTERM, and the ingress may still route to it until its endpoint goes, usually under a second. A POST in that window fails with a 502. The usual fix is a `preStop` sleep of a few seconds, but the image is distroless (no `sleep` binary) and the `lifecycle.preStop.sleep` handler needs Kubernetes 1.30 or newer. Neither is assumed here. On a 1.30+ cluster, add to the server container in `deploy/k8s/room.yaml`:

```yaml
lifecycle:
  preStop:
    sleep: { seconds: 5 }
```

`terminationGracePeriodSeconds` (45) covers the sleep, the 2 s drain and a mail send in flight (30 s).

## Pods and leader

```sh
kubectl -n "$NS" get pods -l app.kubernetes.io/name=grill-room -o wide
for p in $(kubectl -n "$NS" get pods -l app.kubernetes.io/name=grill-room -o name); do
  kubectl -n "$NS" port-forward "$p" 17681:7681 >/dev/null & pf=$!
  sleep 2
  echo "$p $(curl -s http://127.0.0.1:17681/metrics | grep '^grill_room_leader ')"
  kill "$pf"
done
```

Two pods `Running` and `1/1`, on two nodes when the cluster has them. One prints `grill_room_leader 1`, the other `0`. With `make k8s-monitoring`, `sum(grill_room_leader{namespace="<ns>"})` is 1.

## One pod lost (017-AC21)

Start a new watch (step 2), then delete one server pod and wait for its replacement:

```sh
kubectl -n "$NS" get pods -l app.kubernetes.io/name=grill-room -o wide   # pick the leader: grill_room_leader 1 (step 4)
kubectl -n "$NS" delete pod <pod>
kubectl -n "$NS" rollout status deploy/grill-room                        # successfully rolled out: 2 ready again
```

- The replacement downloads the model files again, about 440 MB, before it is ready.
- The other pod serves every request meanwhile: the room page in a browser stays live, as the watch shows.
- A pod delete does not go through the PodDisruptionBudget (`minAvailable: 1`); a node drain would, holding the second eviction until the first replacement is ready.

## Rollback

- Between two releases with two pods (Phase 4 on): `kubectl -n "$NS" rollout undo deploy/grill-room` rolls back the same way, one pod at a time.
- To a release before Phase 4 (one pod, `Recreate`, the `grill-room-models` volume): `rollout undo` does not apply. Scale to one pod, then apply that release's manifests from a worktree, as [cutover.md](cutover.md)'s rollback does:
  ```sh
  kubectl -n "$NS" scale deploy grill-room --replicas=1
  git worktree add ../grill-room-rollback <old tag>
  cp .env ../grill-room-rollback/
  make -C ../grill-room-rollback k8s-apply GRILL_ROOM_IMAGE=<old image>
  kubectl -n "$NS" delete pdb grill-room
  git worktree remove --force ../grill-room-rollback
  ```
  Its manifests mount `grill-room-models` again, so that volume must still exist (below). `make k8s-apply` does not prune: the PodDisruptionBudget stays, and with `minAvailable: 1` on one pod it blocks every node drain, hence its deletion.
- For the cutover's rollback to SQLite, see [cutover.md](cutover.md).

## After the first rollout to two pods

Releases before it mounted a ReadWriteOnce `grill-room-models` volume. `make k8s-apply` does not prune, so it stays in the cluster, unused. A rollback to such a release mounts it again: delete it only once the first rollout to two pods is confirmed and no rollback to an older release is planned:

```sh
kubectl -n "$NS" delete pvc grill-room-models
```

The `grill-room-data` volume (SQLite, before the cutover) is deleted by hand once the cutover's rollback window ends: [cutover.md](cutover.md), "After the window".
