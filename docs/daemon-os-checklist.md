# Daemon checks on macOS, Windows and WSL

On Linux the daemon is tested for real. macOS, native Windows and WSL get cross-compiled builds and unit tests with faked platform calls (Keychain runner, WSL detection, browser opener). CI's `go` job cross-compiles the six release targets (`make release`), and its `windows` job runs the unit tests on `windows-latest` (003-AC30). Run these steps on a real machine of each kind before merging. Each one names the acceptance criterion it covers.

Every step spends nothing except step 3, which makes one `haiku` request.

## Prerequisites

- Docker running: Docker Desktop on macOS; Docker Desktop with WSL integration, or Docker Engine inside the WSL distro.
- Claude Code installed and logged in on that machine (in the WSL distro for WSL).
- The daemon build for the machine: `make release` gives `dist/grill-room-daemon-darwin-arm64` (Apple silicon), `-darwin-amd64` (Intel) or `-linux-amd64` / `-linux-arm64` (WSL).
- No sandbox image to prepare: the daemon pulls the room server's or builds the Dockerfile it embeds (a few minutes the first time).
- A room on a reachable room server with you joined in a browser on that machine.

## Steps

1. **Credentials source (AC23).** Run `grill-room-daemon check` with Docker running. The `account default:` line must say `from Keychain` on macOS and `from <home>/.claude/.credentials.json` on WSL.
   - With `CLAUDE_CONFIG_DIR` set to another logged-in config dir, the line must name that dir on WSL. On macOS, Claude Code stores that config dir's token in a Keychain item named `Claude Code-credentials-<8 hex>`; the line must still say `from Keychain`.
2. **Locked Keychain, macOS only (AC24).** Lock the login keychain (`security lock-keychain ~/Library/Keychains/login.keychain-db`), move any `~/.claude/.credentials.json` aside, and start the daemon. Within 5 s the page's account `default` must show "unreadable" with `macOS Keychain: no answer within 3s, the Keychain may be locked...`, a badge on the Accounts tab with the warning at the top of that tab, and "Hosting the agent" unavailable. Unlock, press "Check again": both turn green. Put the file back.
3. **Sandbox turn, isolation, no token (AC25, AC26, DoD).** `grill-room-daemon check` must print `sandbox turn: "ok"`, `isolation: ok` and `real token matches: filesystem 0, volumes 0, inspect 0`, and exit 0.
4. **Host files untouched (AC27).** Before step 3, note `stat` of `~/.claude/.credentials.json` (if present) and, on macOS, `security find-generic-password -s "Claude Code-credentials" | grep mdat`. Both must be unchanged after step 3. Do not compare `~/.claude.json`: a running Claude Code session rewrites it on its own.
5. **Browser and pairing (AC22).** Run `grill-room-daemon https://<host>/r/<room>`. The page must open in the default browser (on WSL, the Windows browser, through `wslview` or `explorer.exe`). Follow "approve in the room", check the code matches, press Approve. The page must reach "Paired. Connected." with no token shown anywhere.
6. **Docker down (AC28).** Quit Docker Desktop (or stop the engine) and press "Check again". The page must say "Docker is not running" and "Hosting the agent: unavailable". Restart Docker and check again.
7. **Stop.** Press "Stop the daemon". The process must exit, and `docker ps -a --filter label=grill-room-daemon` must list nothing.
8. **Download panel (AC36).** Open the room page on the machine (on WSL, in the Windows browser) and unfold "Host the agent". It must preselect macOS on a Mac (ARM64 in Chrome on Apple silicon, x86-64 in Chrome on Intel; Safari and Firefox always show ARM64) and Windows (WSL) on Windows, with the line about the WSL terminal. The sha256 shown must match `shasum -a 256` (macOS) or `sha256sum` (WSL) of the file the link downloads.
9. **One-liner (AC36).** Move any earlier `~/.local/bin/grill-room-daemon` aside, then paste the panel's one-liner into Terminal (macOS) or a WSL terminal. It must print `Installed <home>/.local/bin/grill-room-daemon` and open the daemon page, as in step 5. With cosign on the PATH and a room server shipping a signed release, it must also print `Signature checked: built by the grill-room release workflow.`; without cosign, `Signature not checked: cosign is not installed.` WSL must also print `WSL detected: installing the Linux build.` `file ~/.local/bin/grill-room-daemon` must name the machine's CPU. On Apple silicon, run it once more from a Rosetta shell (`arch -x86_64 /bin/zsh`): it must still install the arm64 build.
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

18. **Setup token storage (003-AC12).** Add an account of kind "a setup token" on the page, with a token from `claude setup-token`. It must show "valid" and "in the OS keyring". The item must exist, named `grill-room-<8 hex>` for the account name: Keychain Access on macOS, `cmdkey /list` on Windows (target `grill-room-<8 hex>:<name>`), `secret-tool search service grill-room-<8 hex>` on Linux with a Secret Service. No file under the state root may hold the token. Remove the account: the item must be gone.
19. **Owner-only files on Windows (003-AC12).** `icacls %LOCALAPPDATA%\grill-room\instances\<id>\tokens.json` must list one entry, the current user with `(F)`, and no inherited entry. The CI runs the same check (`TestOwnerOnlyDACL`), and the Credential Manager round trip (`TestCredentialManager`).
20. **Accounts and cut-off (003-AC13, 003-AC15).** Pick the setup token account for a room on the page and press a persona: the account's request count must grow. Press "Cut it off from Claude" during a turn: the room must show "the host cut this agent off from Claude for now" and the agent offline, until "Let it use Claude again". One haiku request.

## Platforms (003-AC30)

Three haiku requests: step 22 runs one turn, and each `check` in step 25 runs one sandbox turn. Write what you saw next to each step: several of them answer open questions.

### macOS

21. **Keychain account.** With Claude Code logged in, start the supervisor and open its page. Account `default` (the config dir `~/.claude`) must say "valid" and "read from Keychain". Add a second account of kind "a config dir" pointing at another dir logged in with `CLAUDE_CONFIG_DIR=<dir> claude`: it must say the same, and `security find-generic-password -s 'Claude Code-credentials-<8 hex>'` must find its item. The first read may raise a Keychain prompt for `grill-room-daemon`; note whether "Always Allow" sticks across a supervisor update (a new binary may ask again).
22. **Docker Desktop.** Pair a room and press a persona. The turn must run in a sandbox (`docker ps --filter label=grill-room-root` while it runs) and answer. Quit Docker Desktop: the page must say "Docker is not running" within one check.
23. **LaunchAgent.** Step 15 on macOS: `~/Library/LaunchAgents/io.github.babs.grill-room.plist` must exist with the binary's full path, and `launchctl print gui/$(id -u)/io.github.babs.grill-room` must show it loaded after a new login.
24. **Gatekeeper on a browser download.** Download the macOS binary from the room page's "Host the agent" panel in Safari, `chmod +x` it and run it from Terminal. Note what Gatekeeper says (the binary is not notarized: expected "cannot be opened because the developer cannot be verified"). `xattr -d com.apple.quarantine <file>` must make it run. The one-liner (step 9) must run with no prompt, since `curl` sets no quarantine flag.

### Native Windows

Use PowerShell, not an elevated one, with Docker Desktop running and `grill-room-daemon-windows-<arch>.exe` downloaded from the room page or the GitHub release.

25. **Claude Code credentials location (spec 003, open question 1).** Log in with `claude` in PowerShell, then run:

    ```powershell
    Test-Path $env:USERPROFILE\.claude\.credentials.json
    .\grill-room-daemon-windows-amd64.exe check
    ```

    Expected: `True`, and `account default: ok, from C:\Users\<you>\.claude\.credentials.json`. Then log in with another config dir and check that Claude Code honours it the same way:

    ```powershell
    $env:CLAUDE_CONFIG_DIR = "$env:USERPROFILE\claude-alt"; claude   # log in, then /exit
    Test-Path $env:USERPROFILE\claude-alt\.credentials.json
    .\grill-room-daemon-windows-amd64.exe check
    Remove-Item Env:CLAUDE_CONFIG_DIR
    ```

    Expected: `True`, and `from C:\Users\<you>\claude-alt\.credentials.json`. If either `Test-Path` says `False`, look for the file with `Get-ChildItem $env:USERPROFILE -Recurse -Filter .credentials.json -Force -ErrorAction SilentlyContinue` and `cmdkey /list | Select-String -Pattern claude`, and write down where Claude Code keeps it: the account picker reads only the file.
26. **Credential Manager.** Step 18 on Windows: `cmdkey /list` must show `grill-room-<8 hex>:<name>` while the setup token account exists, and not after it is removed.
27. **Docker Desktop.** Step 25's `check` runs must have printed `sandbox turn: "ok"`, `isolation: ok` and `real token matches: filesystem 0, volumes 0, inspect 0`. Note Docker Desktop's backend (WSL 2 or Hyper-V) if they failed.
28. **Scheduled task and elevation.** Step 15 from a non-elevated PowerShell. Note whether "Start at login" worked or the page showed `schtasks` failing with "Access is denied". If it needed elevation, note whether `schtasks /Create /SC ONLOGON /RL LIMITED /TN grill-room /TR <exe>` works from an elevated shell and the task then starts the supervisor at login as the normal user.
29. **Named pipe.** With the supervisor running, `Get-ChildItem \\.\pipe\ | Where-Object Name -like 'grill-room-*'` must list one pipe; note its name. Step 13 must go through it. Then check that a pipe another user created first is refused. Stop the supervisor. From a second local account (`runas /user:<other> powershell`, Windows PowerShell 5.1), hold that pipe name with a DACL that lets anyone connect:

    ```powershell
    $sec = New-Object System.IO.Pipes.PipeSecurity
    $sec.AddAccessRule((New-Object System.IO.Pipes.PipeAccessRule('Everyone', 'ReadWrite', 'Allow')))
    $s = New-Object System.IO.Pipes.NamedPipeServerStream('grill-room-<hex>', 'InOut', 1, 'Byte', 'None', 4096, 4096, $sec)
    $s.WaitForConnection(); (New-Object System.IO.StreamReader($s)).ReadLine()
    ```

    As yourself, start the supervisor with no argument: its log must show `handoff_socket_failed`. In a second terminal, run it with a room link. It must exit at once with `refusing to hand over: pipe served by another user: process <pid> runs as <SID>` (or `... process <pid> unreadable`), and the other account's `ReadLine()` must return nothing.

### WSL

WSL runs the Linux build and counts as Linux: steps 1 to 20 apply as written for WSL. Check that install.sh printed `WSL detected: installing the Linux build.`, that paths follow XDG inside the distro (step 12), and that autostart uses a systemd user unit when the distro runs systemd.

## Known risks to watch

- A locked Secret Service (Linux) or Keychain (macOS) may show an unlock prompt. After 5 s the supervisor gives up and keeps a new setup token in its owner-only file instead; the page says where the token is.

- WSL with Docker Desktop: the proxy listens on `127.0.0.1` inside the distro and the relay reaches it through `host-gateway`. If step 3 fails with a connection error from the relay, note which Docker setup the machine runs.
- A host firewall that blocks containers from reaching the host's bridge address breaks the relay on native engines. Step 3 shows it as a failed turn.
- Rootless Docker: the bridge gateway is not a host address, so the proxy cannot listen there. The daemon reports it at the first turn.
- macOS Gatekeeper: `curl` sets no quarantine flag, so the one-liner's binary runs. A binary downloaded through the browser carries `com.apple.quarantine`, and Gatekeeper refuses it because it is unsigned. If step 8's download will not start, `xattr -d com.apple.quarantine <file>` is the workaround to note.
