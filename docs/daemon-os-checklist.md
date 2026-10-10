# Daemon checks on macOS and WSL

On Linux the daemon is tested for real. macOS and WSL get cross-compiled builds and unit tests with faked platform calls (Keychain runner, WSL detection, browser opener). Run these steps on a real machine of each kind before merging. Each one names the acceptance criterion it covers.

Every step spends nothing except step 3, which makes one `haiku` request.

## Prerequisites

- Docker running: Docker Desktop on macOS; Docker Desktop with WSL integration, or Docker Engine inside the WSL distro.
- Claude Code installed and logged in on that machine (in the WSL distro for WSL).
- The daemon build for the machine: `make release` gives `dist/grill-room-daemon-darwin-arm64` (Apple silicon), `-darwin-amd64` (Intel) or `-linux-amd64` / `-linux-arm64` (WSL).
- No sandbox image to prepare: the daemon pulls the room server's or builds the Dockerfile it embeds (a few minutes the first time).
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
8. **Download panel (AC36).** Open the room page on the machine (on WSL, in the Windows browser) and unfold "Host the agent". It must preselect macOS on a Mac (ARM64 in Chrome on Apple silicon, x86-64 in Chrome on Intel; Safari and Firefox always show ARM64) and Windows (WSL) on Windows, with the line about the WSL terminal. The sha256 shown must match `shasum -a 256` (macOS) or `sha256sum` (WSL) of the file the link downloads.
9. **One-liner (AC36).** Move any earlier `~/.local/bin/grill-room-daemon` aside, then paste the panel's one-liner into Terminal (macOS) or a WSL terminal. It must print `Installed <home>/.local/bin/grill-room-daemon` and open the daemon page, as in step 5. WSL must also print `WSL detected: installing the Linux build.` `file ~/.local/bin/grill-room-daemon` must name the machine's CPU. On Apple silicon, run it once more from a Rosetta shell (`arch -x86_64 /bin/zsh`): it must still install the arm64 build.
10. **Sandbox image.** With `GRILL_ROOM_SANDBOX_IMAGE` unset on the machine, the page's "Sandbox image" line must go through pulling (the room server advertises an image and this Docker can pull it) or building (otherwise), then show `ready (...)`, and "Hosting the agent" must turn available. Note which of the two happened and how long it took.
11. **Self-update (AC37).** Have the room server hand out a newer release (redeploy it with a higher `VERSION`). Within a minute the page must offer "Update and restart" with the new version. Press it: a new page must open showing the new version and "Paired. Connected.", and the checksum of `~/.local/bin/grill-room-daemon` must equal the room's `/download/grill-room-daemon-<os>-<arch>.sha256`.

## Known risks to watch

- WSL with Docker Desktop: the proxy listens on `127.0.0.1` inside the distro and the relay reaches it through `host-gateway`. If step 3 fails with a connection error from the relay, note which Docker setup the machine runs.
- A host firewall that blocks containers from reaching the host's bridge address breaks the relay on native engines. Step 3 shows it as a failed turn.
- Rootless Docker: the bridge gateway is not a host address, so the proxy cannot listen there. The daemon reports it at the first turn.
- macOS Gatekeeper: `curl` sets no quarantine flag, so the one-liner's binary runs. A binary downloaded through the browser carries `com.apple.quarantine`, and Gatekeeper refuses it because it is unsigned. If step 8's download will not start, `xattr -d com.apple.quarantine <file>` is the workaround to note.
