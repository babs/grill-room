# Daemon checks on macOS and WSL

On Linux the daemon is tested for real. macOS and WSL get cross-compiled builds and unit tests with faked platform calls (Keychain runner, WSL detection, browser opener). Run these steps on a real machine of each kind before merging. Each one names the acceptance criterion it covers.

Every step spends nothing except step 3, which makes one `haiku` request.

## Prerequisites

- Docker running: Docker Desktop on macOS; Docker Desktop with WSL integration, or Docker Engine inside the WSL distro.
- Claude Code installed and logged in on that machine (in the WSL distro for WSL).
- The daemon build for the machine: `make build-daemons` gives `bin/grill-room-daemon-darwin-arm64` (Apple silicon), `-darwin-amd64` (Intel) or `-linux-amd64` / `-linux-arm64` (WSL).
- The sandbox image on that machine's Docker: `make sandbox-image`, or `docker save` it on the build host and `docker load` it there.
- A room on a reachable room server with you joined in a browser on that machine.

## Steps

1. **Credentials source (AC23).** Run `grill-room-daemon check` with Docker running. The `credentials:` line must say `from Keychain` on macOS and `from <home>/.claude/.credentials.json` on WSL.
   - With `CLAUDE_CONFIG_DIR` set to another logged-in config dir, the line must name that dir on WSL. On macOS, Claude Code stores that config dir's token in a Keychain item named `Claude Code-credentials-<8 hex>`; the line must still say `from Keychain`.
2. **Locked Keychain, macOS only (AC24).** Lock the login keychain (`security lock-keychain ~/Library/Keychains/login.keychain-db`), move any `~/.claude/.credentials.json` aside, and start the daemon. Within 5 s the page's "Claude credentials" line must read `macOS Keychain: no answer within 3s, the Keychain may be locked...` and "Hosting the agent" must say unavailable. Unlock, press "Check again": both turn green. Put the file back.
3. **Sandbox turn, isolation, no token (AC25, AC26, DoD).** `grill-room-daemon check` must print `sandbox turn: "ok"`, `isolation: ok` and `real token matches: filesystem 0, volumes 0, inspect 0`, and exit 0.
4. **Host files untouched (AC27).** Before step 3, note `stat` of `~/.claude/.credentials.json` (if present) and, on macOS, `security find-generic-password -s "Claude Code-credentials" | grep mdat`. Both must be unchanged after step 3. Do not compare `~/.claude.json`: a running Claude Code session rewrites it on its own.
5. **Browser and pairing (AC22).** Run `grill-room-daemon https://<host>/r/<room>`. The page must open in the default browser (on WSL, the Windows browser, through `wslview` or `explorer.exe`). Follow "approve in the room", check the code matches, press Approve. The page must reach "Paired. Connected." with no token shown anywhere.
6. **Docker down (AC28).** Quit Docker Desktop (or stop the engine) and press "Check again". The page must say "Docker is not running" and "Hosting the agent: unavailable". Restart Docker and check again.
7. **Stop.** Press "Stop the daemon". The process must exit, and `docker ps -a --filter label=grill-room-daemon` must list nothing.

## Known risks to watch

- WSL with Docker Desktop: the proxy listens on `127.0.0.1` inside the distro and the relay reaches it through `host-gateway`. If step 3 fails with a connection error from the relay, note which Docker setup the machine runs.
- A host firewall that blocks containers from reaching the host's bridge address breaks the relay on native engines. Step 3 shows it as a failed turn.
- Rootless Docker: the bridge gateway is not a host address, so the proxy cannot listen there. The daemon reports it at the first turn.
