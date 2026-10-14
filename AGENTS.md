# Installing court-gateway — instructions for AI agents

You are installing court-gateway for a sports club on a machine you can run commands on. Follow
these steps in order. After every step run its **Verify** command and compare the result with
**Expect**. If a step fails twice, stop and report the step, the command and the error code to the
human. Do not improvise around a failure.

## Rules (non-negotiable)

1. **Ask the human, do not guess**, for exactly these inputs:
   - join code (one-time, format `XXXX-XXXX-XXXX-XXXX`) — the club gets ONE code from its partner,
     e.g. shown in the partner's admin panel;
   - Viva administrator login (e-mail) and **password**;
   - the hub URL and the hub key fingerprint (`XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XX`, given with the
     join code) **only** if the binary is self-built (`version` says `selfbuilt`) or `setup`
     answers `MISSING_INPUT` naming `--hub-url` or `--hub-key-fingerprint`: a release knows its
     hub and that hub's key.

   Nothing else. The club does **not** choose a club id, courts, hours, a studio or a time zone:
   one installation is one CRM account, and the operator configures its clubs on the hub. Never
   write `club`, `venue` or `courts` into `gateway.yaml` (the gateway refuses them).
2. **Never echo, print, log or store the Viva password or the join code** — not in your messages,
   not in files other than the `.env` that `setup` writes, not in shell history. Pass them only as
   environment variables of the single `setup` command, or the password via stdin
   (`--password-stdin`). Do not run commands with `set -x`. Never `cat` the `.env` file.
3. **Never open inbound access.** Do not add firewall rules, port forwarding, a public IP, DNS
   records, TLS certificates, reverse proxies or tunnels of your own. The gateway has no port: it
   only connects **out** to the hub over HTTPS.
4. Do not disable automatic updates, do not edit the service files `install-service` writes, do not
   commit or upload `gateway.yaml` or `.env` anywhere.
5. Use the official release binary and verify its checksum (step 2). Build from source only if the
   human asks.
6. The machine's clock should be roughly right (automatic time on): the gateway tolerates up to an
   hour off the hub; further off it reports `CLOCK_SKEW` — tell the human to fix the clock.

**Success** = `court-gateway doctor --json` exits `0` with no `fail` (a `warn` on
`VERSION_CURRENT` or `SECRETS_FILE` is acceptable; `CRM_OCCUPANCY` is `skip` until the operator
has added clubs) **and** `TUNNEL_CONNECTED` is `ok` — the hub sees the installation. After that
the club side never has to do anything again, not even when the operator adds a club.

## Step 0 — collect inputs

Ask the human for the inputs in rule 1. Best: let the human type the secrets into the terminal
you share, without echo (works in bash and zsh):

```sh
printf 'Join code: ';          stty -echo; read -r GATEWAY_JOIN_CODE; stty echo; echo
printf 'Viva login (e-mail): '; read -r CRM_USERNAME
printf 'Viva password: ';      stty -echo; read -r CRM_PASSWORD; stty echo; echo
export GATEWAY_JOIN_CODE CRM_USERNAME CRM_PASSWORD
# only for a self-built binary: printf 'Hub URL: '; read -r GATEWAY_HUB_URL; export GATEWAY_HUB_URL
#   printf 'Hub key fingerprint: '; read -r GATEWAY_HUB_KEY_FINGERPRINT; export GATEWAY_HUB_KEY_FINGERPRINT
```

```powershell
# Windows PowerShell (run as Administrator)
$env:GATEWAY_JOIN_CODE = [Net.NetworkCredential]::new('', (Read-Host 'Join code' -AsSecureString)).Password
$env:CRM_USERNAME      = Read-Host 'Viva login (e-mail)'
$env:CRM_PASSWORD      = [Net.NetworkCredential]::new('', (Read-Host 'Viva password' -AsSecureString)).Password
```

If the human can only give you the values in the conversation, put them into these environment
variables of the `setup` command in step 3 and nowhere else; never repeat them back.

**Verify:** `test -n "$CRM_PASSWORD" && test -n "$GATEWAY_JOIN_CODE" && echo inputs-ok`
(PowerShell: `if ($env:CRM_PASSWORD -and $env:GATEWAY_JOIN_CODE) { 'inputs-ok' }`).
**Expect:** `inputs-ok`. Never print the values themselves.

## Step 1 — platform and folder

| OS | Folder | Release file |
|---|---|---|
| Linux x86-64 | `~/court-gateway` | `court-gateway-<v>-linux-amd64` |
| Linux ARM64 | `~/court-gateway` | `court-gateway-<v>-linux-arm64` |
| macOS Apple Silicon | `~/court-gateway` | `court-gateway-<v>-darwin-arm64` |
| macOS Intel | `~/court-gateway` | `court-gateway-<v>-darwin-amd64` |
| Windows x64 | `C:\court-gateway` | `court-gateway-<v>-windows-amd64.exe` |
| Windows ARM64 | `C:\court-gateway` | `court-gateway-<v>-windows-arm64.exe` (the x64 build also runs under emulation on Windows 11 ARM; prefer arm64) |

Detect: `uname -s -m` (Linux/macOS) or `$env:PROCESSOR_ARCHITECTURE` (Windows). Create the folder
and `cd` into it. (On Windows `setup` closes the folder and `.env` to everyone but Administrators and SYSTEM
itself, and stops before writing any secret if it cannot.)

**Verify:** `pwd` / `Get-Location`. **Expect:** the folder from the table.

## Step 2 — download and verify

Release page: `https://github.com/Padelapp-Club/court-gateway/releases/latest/download/<file>`.
Set `F` to the release file name for the platform and run the one command for the OS: it downloads
`SHA256SUMS` and the file, and renames the file (so it can be run) **only** if the download worked
and its checksum matches — each step runs only if the previous one succeeded.

Linux:

```sh
F=court-gateway-<v>-linux-amd64
B=https://github.com/Padelapp-Club/court-gateway/releases/latest/download
curl -fLO "$B/SHA256SUMS" && curl -fLO "$B/$F" &&
  grep " $F\$" SHA256SUMS | sha256sum -c - && mv "$F" court-gateway && chmod +x court-gateway && echo checksum-ok
```

macOS:

```sh
F=court-gateway-<v>-darwin-arm64
B=https://github.com/Padelapp-Club/court-gateway/releases/latest/download
curl -fLO "$B/SHA256SUMS" && curl -fLO "$B/$F" &&
  grep " $F\$" SHA256SUMS | shasum -a 256 -c - && mv "$F" court-gateway && chmod +x court-gateway && echo checksum-ok
```

On Windows ARM64 put `windows-arm64.exe` in `$f`.

Windows:

```powershell
& {
  $f = 'court-gateway-<v>-windows-amd64.exe'
  $b = 'https://github.com/Padelapp-Club/court-gateway/releases/latest/download'
  Invoke-WebRequest "$b/SHA256SUMS" -OutFile SHA256SUMS -ErrorAction Stop
  Invoke-WebRequest "$b/$f" -OutFile $f -ErrorAction Stop
  $want = (Select-String -Path SHA256SUMS -Pattern (' ' + [regex]::Escape($f) + '$')).Line -split '\s+' | Select-Object -First 1
  if (-not $want -or (Get-FileHash $f -Algorithm SHA256).Hash -ne $want) { throw "CHECKSUM MISMATCH: do not run $f" }
  Move-Item -Force $f court-gateway.exe
  'checksum-ok'
}
```

Anything but `checksum-ok` (a `FAILED` line, `CHECKSUM MISMATCH`, no output): **stop**, delete the
downloaded file, never run it, and report to the human.

On macOS, if the binary is quarantined: `xattr -d com.apple.quarantine court-gateway`.

**Verify:** `./court-gateway version` (Windows: `.\court-gateway.exe version`).
**Expect:** `court-gateway <v> (release, <os>-<arch>)`. `selfbuilt` instead of `release` means a
binary without the release key: it works but never updates itself — tell the human.

## Step 3 — setup

```sh
./court-gateway setup --non-interactive --json \
  --crm-username "$CRM_USERNAME" --config "$PWD/gateway.yaml" > setup-result.json
```

(`CRM_PASSWORD`, `GATEWAY_JOIN_CODE` and, for a self-built binary, `GATEWAY_HUB_URL` and
`GATEWAY_HUB_KEY_FINGERPRINT` are read from the environment; `--crm viva` is the default. Windows: `.\court-gateway.exe`, `--config C:\court-gateway\gateway.yaml`, line
continuation with a backtick. There are no flags for a club id, studio, time zone or grid.)

Then remove the secrets from your shell: `unset CRM_PASSWORD GATEWAY_JOIN_CODE`
(PowerShell: `Remove-Item Env:CRM_PASSWORD, Env:GATEWAY_JOIN_CODE`).

**Verify:** read `setup-result.json` (it never contains secrets).
**Expect:** `"ok": true` and `installation.id` — the installation the hub assigned. Tell the
human the installation name and any `warnings`. Do not edit `gateway.yaml`: the clubs come from
the hub.

If `"ok": false`, act on `error.code`:

| Code | Action |
|---|---|
| `MISSING_INPUT` | an input from step 0 is empty — ask the human again (`--hub-url` / `--hub-key-fingerprint`: the binary is self-built, ask for the hub URL and the fingerprint) |
| `INVALID_INPUT` | an input is malformed (hub URL, CRM type) — the message says which |
| `CRM_AUTH_FAILED` | wrong Viva login or password — ask the human to re-enter; the join code is **not** spent |
| `CRM_INVALID_RESPONSE` / `CRM_UNREACHABLE` | Viva answers oddly or not at all — check `curl -sI https://api.vivacrm.ru`, retry later |
| `JOIN_CODE_REJECTED` | code wrong, used or expired — ask the human for a new code |
| `HUB_UNREACHABLE` | check the hub URL and outbound HTTPS; never open inbound ports |
| `HUB_RATE_LIMITED` | too many wrong codes from this address — wait a minute, then retry once with a code the human re-checks |
| `CONFIG_EXISTS` | a previous install exists — ask the human before re-running with `--force` |
| `FOLDER_NOT_RESTRICTED` | Windows: the folder could not be closed to everyone but Administrators and SYSTEM — re-run setup in PowerShell as Administrator; nothing was written and the join code is **not** spent |

## Step 4 — install the service

| OS | Command | Verify | Expect |
|---|---|---|---|
| Linux | `sudo ./court-gateway install-service` | `systemctl is-active court-gateway` | `active` |
| macOS (club) | `sudo ./court-gateway install-service` | `sudo launchctl print system/court-gateway \| grep state` | `state = running` |
| macOS (trial, no sudo) | `./court-gateway install-service` | `launchctl print gui/$(id -u)/court-gateway \| grep state` | `state = running` |
| Windows | `.\court-gateway.exe install-service` (Administrator) | `sc.exe query court-gateway` | `STATE : 4 RUNNING` |

On macOS always use the club row (a system service: starts at boot). Without `sudo` the gateway
becomes a per-user LaunchAgent that runs only while that user is logged in — tell the human it is
for a trial only; a later `sudo … install-service` replaces it.

On a cloud VM without a regular user, add `--run-as <user>` with a dedicated user that owns the
folder (the folder must stay writable by that user: updates are kept in its `state/versions/`;
the installed binary itself is never replaced).

## Step 5 — doctor

Wait 30 seconds, then:

```sh
./court-gateway doctor --json -config "$PWD/gateway.yaml"; echo "exit=$?"
```

**Expect:** `exit=0`, `"ok": true`, and these checks `ok`: `CONFIG_VALID`, `CRM_LOGIN`,
`HUB_REACHABLE`, `HUB_AUTH`, `TUNNEL_CONNECTED`, `HUB_CONFIG`. `CRM_OCCUPANCY` is `ok` once the
operator has added a club and `skip` before. The JSON also carries `installation`,
`config_version`, `clubs` (on the hub) and `clubs_served` (by the running service). For any
`fail`, follow its `hint`; the codes are stable:

| Code | Meaning when failing |
|---|---|
| `CONFIG_VALID` | `gateway.yaml` or `.env` is broken — the message lists every problem |
| `CRM_LOGIN` / `CRM_OCCUPANCY` | the Viva login is wrong, a club's studio/rooms on the hub are wrong, or Viva is unreachable |
| `HUB_REACHABLE` | no outbound HTTPS to the hub, or its certificate failed verification |
| `HUB_AUTH` | the hub does not accept this gateway — a new join code and `setup --force` are needed |
| `TUNNEL_CONNECTED` | the service is not running or not connected yet — check step 4, wait a minute |
| `HUB_CONFIG` | `fail`: the clubs configured on the hub do not load with this CRM — report to the operator (the service keeps the previous config); `warn`: the service has not applied the newest config yet, or a club is served but its config disagrees with the CRM (its time zone) — report to the operator |
| `SECRETS_FILE` | Windows `fail`: the folder holding `.env`, or `.env`, is open to other users — run the `hint` command (`install-service`, which closes both) in PowerShell as Administrator; `warn` elsewhere: `chmod 600 .env` |
| `VERSION_CURRENT` | `warn` only: self-built binary, or an update is pending (installs itself) |

## Step 6 — report

Tell the human: installed version, installation id, the clubs it serves (if the operator has
added any already), that nothing else is needed on their side, where the log is (Linux `journalctl -u court-gateway`, macOS/Windows `court-gateway.log` next
to `gateway.yaml`; on macOS, messages from before the log starts are in `court-gateway.stderr.log`), and that the operator can confirm the installation is connected in the hub. Do not
include any secret in the report.

## The Viva password changed

No join code is needed. Have the human type the new password as in step 0 (`CRM_PASSWORD` only),
then, in the install folder:

```sh
./court-gateway set-password -config "$PWD/gateway.yaml"; echo "exit=$?"
```

(Windows, PowerShell as Administrator: `.\court-gateway.exe set-password -config "$PWD\gateway.yaml"`.)
It logs in to Viva with the new password first and replaces only `CRM_PASSWORD` in `.env`.
**Expect:** `exit=0`; `CRM_AUTH_FAILED` means the password is wrong — ask again. The running
service picks the new password up within a minute, no restart (in Docker: recreate the container
as it prints). After a rejected password the service makes no CRM call at all until this is done.
Then `unset CRM_PASSWORD` and run step 5.

## Uninstall

`sudo ./court-gateway uninstall-service` (Windows: `.\court-gateway.exe uninstall-service`), then
delete the folder. On macOS it removes whichever exists: the system service and the per-user one.
