# Daemon checks on macOS, Windows and WSL

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
11. **Self-update (AC37, 003-AC5).** With a supervisor older than the latest GitHub release, the page must offer "Update and restart" with that version, from GitHub. Press it: a new page must open showing the new version, "project-signed" next to it, and "Paired. Connected.". The checksum of the binary must equal the release's `.sha256`, and `<binary>.sigstore.json` must sit beside it.

## Supervisor (spec 003)

Run these on macOS, on native Windows (`grill-room-daemon-windows-<arch>.exe` from PowerShell, Docker Desktop running) and in WSL. No Claude request.

12. **Paths (003-AC11).** Start the supervisor with a room link and pair. `tokens.json` must sit in `~/.local/state/grill-room/instances/<id>/` (WSL), `~/Library/Application Support/grill-room/instances/<id>/` (macOS) or `%LOCALAPPDATA%\grill-room\instances\<id>\` (Windows), and `instance.json` in `~/.config/grill-room/...`, the same Application Support dir, or `%APPDATA%\grill-room\...`.
13. **Second launch (003-AC6).** With the supervisor running, run the command again with another room link in a second terminal. It must print `handed <room> to the running supervisor`, open the same admin page, and exit, and the page must show the new room. Run it once more with no argument: it must exit with `a supervisor already runs for <dir>`. On Windows, this goes through the named pipe.
14. **Worker restart (003-AC8).** Kill the worker from the page's instance list (`kill <pid>`, or `taskkill /F /PID <pid>`). The page must show a restart and the room "Paired. Connected." again within a few seconds.
15. **Autostart (003-AC11).** Tick "Start at login" on the page, log out and back in: the supervisor must be running (`systemctl --user status grill-room` on WSL with systemd, `launchctl list | grep grill-room` on macOS, `schtasks /Query /TN grill-room` on Windows). Untick it: the unit, `~/Library/LaunchAgents/io.github.babs.grill-room.plist` or the task must be gone, and the running supervisor must keep running. Note whether `schtasks /Create /SC ONLOGON` needed an elevated shell.
16. **Migration (003-AC10).** On a machine with a paired single-room daemon (an earlier release), stop it and start the supervisor. The old dir must be in `legacy-backup-<date>` under the config root, and the room must reconnect without a new pairing.
17. **Worker binary (003-AC2, 003-AC7).** Pair with a room on an instance serving another signed release than the supervisor's. The instance list must show that version and "signed by the project", and the binary must sit in `bin/<version>/` under the state root (`%LOCALAPPDATA%\grill-room\bin\` on Windows). Redeploy that instance with the supervisor's version: once its worker reconnects, `bin/<version>/` must be gone (on Windows, at the next start if the old worker still held the file).

## Known risks to watch

- WSL with Docker Desktop: the proxy listens on `127.0.0.1` inside the distro and the relay reaches it through `host-gateway`. If step 3 fails with a connection error from the relay, note which Docker setup the machine runs.
- A host firewall that blocks containers from reaching the host's bridge address breaks the relay on native engines. Step 3 shows it as a failed turn.
- Rootless Docker: the bridge gateway is not a host address, so the proxy cannot listen there. The daemon reports it at the first turn.
- macOS Gatekeeper: `curl` sets no quarantine flag, so the one-liner's binary runs. A binary downloaded through the browser carries `com.apple.quarantine`, and Gatekeeper refuses it because it is unsigned. If step 8's download will not start, `xattr -d com.apple.quarantine <file>` is the workaround to note.
