# Lab cutover from SQLite to PostgreSQL

Stop, copy, start (spec 017, Q9 and Q10): minutes of downtime, a 24 h rollback window.

Before you start:

- The database exists, empty, with `C` collation and owned by its login role (README, Run). The Secret `K8S_PG_SECRET` holds that role, and the role may create databases: steps 6 and 9 restore into scratch ones.
- `.env` holds the `K8S_PG_*` keys and `GRILL_ROOM_IMAGE` of the PostgreSQL release (`.env.example`).
- The running image's tag is the git tag it was built from (`make k8s-push`), and that tag is in this clone.
- This machine has `docker`, `go`, `sqlite3`, `curl` and `jq`.
- The database accepts TLS and the role's login from the port-forward: step 2 checks it before anything stops.

Run every step in one shell, from a `cutover/` directory at the root of the checkout of the PostgreSQL release. The shell holds the database password and cookies, and `cutover/` holds dumps with personal data: use a private terminal and close it at the end. Load the lab settings first. Only the `K8S_*` keys and the admin token are read, as other `.env` values are not shell-quoted. `pgc` runs the PostgreSQL client tools at the server's major version, in a container that sees `cutover/`:

```sh
mkdir -p -m 700 cutover && cd cutover
set -a; . <(grep -E '^(K8S_[A-Z_]+|GRILL_ROOM_ADMIN_TOKEN)=' ../.env); set +a
export KUBECONFIG=<lab kubeconfig>
NS=${K8S_NAMESPACE:-grill-room}
PG_MAJOR=<the PostgreSQL server's major version>
pgc() { docker run --rm --network host -u "$(id -u):$(id -g)" -v "$PWD:/w" -w /w -e PGUSER -e PGPASSWORD -e PGSSLMODE=require "postgres:$PG_MAJOR-alpine" "$@"; }
```

1. Announce the downtime and the rollback window. Writes made on PostgreSQL inside the window are lost if it rolls back.
2. Record the running image and the git tag it was built from, then take the read-only check that step 8 compares against. The probes need a member's room id and, from that member's browser, the `grill_room_session` cookie of the room and the `grill_room_profile` cookie. Paste each cookie at its prompt, never on a command line.
   ```sh
   for d in grill-room stt tts; do kubectl -n "$NS" get deploy "$d" -o jsonpath='{.spec.template.spec.containers[0].image}' > "before.$d.image"; done
   cp before.grill-room.image before.image
   sed -E 's/.*:([^@]+)@.*/\1/' before.image > before.ref
   git rev-parse --verify "$(cat before.ref)^{commit}"
   read -rsp 'room cookie: ' GRILL_ROOM_CHECK_COOKIE; echo
   read -rsp 'profile cookie: ' GRILL_ROOM_CHECK_SESSION; echo
   export GRILL_ROOM_CHECK_ROOM=<room id> GRILL_ROOM_CHECK_COOKIE GRILL_ROOM_CHECK_SESSION
   make -s -C .. lab-check > before.check
   PGPORT_LOCAL=15432
   kubectl -n "$K8S_PG_NAMESPACE" port-forward "svc/${K8S_PG_HOST%%.*}" "$PGPORT_LOCAL":"${K8S_PG_PORT:-5432}" & PF=$!
   export PGUSER=$(kubectl -n "$NS" get secret "$K8S_PG_SECRET" -o jsonpath='{.data.username}' | base64 -d)
   export PGPASSWORD=$(kubectl -n "$NS" get secret "$K8S_PG_SECRET" -o jsonpath='{.data.password}' | base64 -d)
   sleep 2; kill -0 "$PF" && pgc psql -h 127.0.0.1 -p "$PGPORT_LOCAL" -d "$K8S_PG_DATABASE" -Atc 'SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()'
   ```
   The last command prints `t`. A failed port-forward (the local port taken), a refused login or `f` stops the cutover here, with the server still up: pick another `PGPORT_LOCAL` or fix the access first.
   `git rev-parse` prints the commit the rollback goes back to. `make lab-check` runs `tools/lab-check.sh` against `https://grill-room.$K8S_DOMAIN` with the admin token. It prints one JSON line per room, participant and profile, and the library counts, all sorted, with no field that moves on its own. It writes nothing; the probes do refresh the member's last-seen time, which it does not print.
3. Stop the server and back up the SQLite volume with a throwaway pod that mounts it read-only:
   ```sh
   kubectl -n "$NS" scale deploy grill-room --replicas=0
   kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=grill-room --timeout=5m
   kubectl -n "$NS" apply -f - <<'EOF'
   apiVersion: v1
   kind: Pod
   metadata: { name: sqlite-backup }
   spec:
     securityContext: { runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: { type: RuntimeDefault } }
     containers:
       - name: sh
         image: busybox:1.37
         command: [sleep, "3600"]
         securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
         volumeMounts: [{ name: data, mountPath: /data, readOnly: true }]
     volumes:
       - name: data
         persistentVolumeClaim: { claimName: grill-room-data, readOnly: true }
   EOF
   kubectl -n "$NS" wait --for=condition=Ready pod/sqlite-backup
   mkdir backup && kubectl -n "$NS" exec sqlite-backup -- tar cf - -C /data --exclude=./lost+found . | tar xf - -C backup
   kubectl -n "$NS" delete pod sqlite-backup
   sha256sum backup/grill-room.db* > backup.sha256
   ```
   `backup/` now holds `grill-room.db` and, if present, its `-wal` and `-shm`. Keep it untouched for 30 days.
4. Make a checkpointed copy: `db copy` refuses a non-empty `-wal`, and opening the backup itself would change it.
   ```sh
   cp -a backup work
   sqlite3 work/grill-room.db 'PRAGMA wal_checkpoint(TRUNCATE);' 'PRAGMA integrity_check;'
   ```
   The checkpoint prints `0|...`, the check `ok`.
5. Copy through the port-forward opened in step 2:
   ```sh
   kill -0 "$PF"
   DSN="postgres://127.0.0.1:$PGPORT_LOCAL/$K8S_PG_DATABASE?sslmode=require"
   make -C .. build
   ../bin/grill-room db copy --from work/grill-room.db --to "$DSN"
   ../bin/grill-room db verify --from work/grill-room.db --to "$DSN"
   ```
   Both exit 0 and list every table as equal.
6. Test a backup before the server's first start (017-AC23): dump the copy, restore it into a scratch database, and compare it with the checkpointed copy.
   ```sh
   pgc dropdb --if-exists -h 127.0.0.1 -p "$PGPORT_LOCAL" grill_room_restore
   pgc pg_dump -h 127.0.0.1 -p "$PGPORT_LOCAL" -Fc -f copy.dump "$K8S_PG_DATABASE"
   pgc createdb -h 127.0.0.1 -p "$PGPORT_LOCAL" -T template0 -E UTF8 --locale-provider=libc --lc-collate=C --lc-ctype=C grill_room_restore
   pgc pg_restore -h 127.0.0.1 -p "$PGPORT_LOCAL" --no-owner -d grill_room_restore copy.dump
   ../bin/grill-room db verify --from work/grill-room.db --to "postgres://127.0.0.1:$PGPORT_LOCAL/grill_room_restore?sslmode=require"
   pgc dropdb -h 127.0.0.1 -p "$PGPORT_LOCAL" grill_room_restore
   ```
   `db verify` exits 0 and lists every table as equal. Anything else is a failed copy or dump: do not deploy, roll back.
7. Deploy the PostgreSQL image at one replica: `make -C .. k8s-apply`.
8. Check, and compare with step 2 (017-AC9):
   ```sh
   make -s -C .. lab-check > after.check
   diff before.check after.check
   ```
   The diff is empty, except for two kinds of lines. A room's `host_online` is `false` until its paired app reconnects, which it does without pairing again: wait and run it again. The counts line changes when this release ships new presets, starters or pronunciations. Then open rooms, messages, documents and uploads in a browser. `make e2e-lab` runs a whole session.
9. Before the downtime ends, test a dump taken with the server up (017-AC23):
   ```sh
   pgc dropdb --if-exists -h 127.0.0.1 -p "$PGPORT_LOCAL" grill_room_running
   pgc pg_dump -h 127.0.0.1 -p "$PGPORT_LOCAL" -Fc -f running.dump "$K8S_PG_DATABASE"
   pgc createdb -h 127.0.0.1 -p "$PGPORT_LOCAL" -T template0 -E UTF8 --locale-provider=libc --lc-collate=C --lc-ctype=C grill_room_running
   pgc pg_restore -h 127.0.0.1 -p "$PGPORT_LOCAL" --no-owner -d grill_room_running running.dump
   ../bin/grill-room db verify --from work/grill-room.db --to "postgres://127.0.0.1:$PGPORT_LOCAL/grill_room_running?sslmode=require"
   pgc dropdb -h 127.0.0.1 -p "$PGPORT_LOCAL" grill_room_running
   kill "$PF"
   ```
   `pg_restore` exits 0. `db verify` exits 1 naming only tables a running server writes: `settings` (health check and instance id), `participants` (last-seen times, which step 8's probes refresh), and `persona_templates`, `persona_template_versions`, `starters` and `starter_versions` when the first start refreshed changed presets and starters. This proves a dump of the running server restores. It does not prove the dump equal to the source: step 6 does that. Any other table: roll back.

## Rollback

Inside the window, when a check differs or a bug blocks:

1. `kubectl -n "$NS" scale deploy grill-room --replicas=0`.
2. Apply the release recorded in step 2, with the three images it ran. Its manifests mount `grill-room-data` again, untouched since step 3.
   ```sh
   git worktree add ../../grill-room-rollback "$(cat before.ref)"
   cp ../.env ../../grill-room-rollback/
   make -C ../../grill-room-rollback k8s-apply GRILL_ROOM_IMAGE="$(cat before.grill-room.image)" \
     GRILL_STT_IMAGE="$(cat before.stt.image)" GRILL_TTS_IMAGE="$(cat before.tts.image)"
   ```
3. `make -s -C .. lab-check > rollback.check && diff before.check rollback.check`, read as in step 8.

Writes made on PostgreSQL since the cutover are lost. To try again, drop and recreate the database empty: `db copy` refuses one that is not.

## After the window

Delete the old volume (`kubectl -n "$NS" delete pvc grill-room-data`) and its object in `deploy/k8s/room.yaml`. Until then `make k8s-delete` leaves it, unless `K8S_DELETE_SQLITE_PVC=1`. Keep `cutover/backup/` 30 days and delete the rest of `cutover/`: its dumps and `work/` hold personal data. After a rollback, also remove its worktree, which holds a copy of `.env`: `git worktree remove --force ../../grill-room-rollback`.
