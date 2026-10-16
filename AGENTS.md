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
6. The machine's clock should be roughly right (automatic time on): every decision on "now" uses
   the hub's signed time, so up to an hour off is harmless; further off the gateway reports
   `CLOCK_SKEW` and runs nothing the hub sends — tell the human to fix the clock.

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
# only for a self-built binary: $env:GATEWAY_HUB_URL = Read-Host 'Hub URL'
#   $env:GATEWAY_HUB_KEY_FINGERPRINT = Read-Host 'Hub key fingerprint'
```

If the human can only give you the values in the conversation, put them into these environment
variables of the `setup` command in step 3 and nowhere else; never repeat them back.

**Verify:** `test -n "$CRM_PASSWORD" && test -n "$GATEWAY_JOIN_CODE" && echo inputs-ok`
(PowerShell: `if ($env:CRM_PASSWORD -and $env:GATEWAY_JOIN_CODE) { 'inputs-ok' }`).
**Expect:** `inputs-ok`. Never print the values themselves.

## Step 1 — platform and folder

| OS | Folder | Platform |
|---|---|---|
| Linux x86-64 | `~/court-gateway` | `linux-amd64` |
| Linux ARM64 | `~/court-gateway` | `linux-arm64` |
| macOS Apple Silicon | `~/court-gateway` | `darwin-arm64` |
| macOS Intel | `~/court-gateway` | `darwin-amd64` |
| Windows x64 | `C:\court-gateway` | `windows-amd64` |
| Windows ARM64 | `C:\court-gateway` | `windows-arm64` |

Always install the latest release; never ask the human for a version. The commands in step 2
detect the platform themselves (`uname -s`, `uname -m`; Windows `$env:PROCESSOR_ARCHITECTURE`).
Linux/macOS: create the folder and `cd` into it; step 2 closes it to other users (`chmod 700`).
Windows: step 2 creates `C:\court-gateway` and closes it to everyone but Administrators and SYSTEM
before downloading anything (a folder that already exists, is open to others and holds files other
than the release's stops it with `FOREIGN FILES IN AN OPEN FOLDER`: ask the human before renaming
it); then `cd C:\court-gateway`. (`setup` checks the folder and `.env` again, and stops before
writing any secret if they are open.)

**Verify:** `pwd` (Windows: after step 2, `Get-Location`). **Expect:** the folder from the table.

## Step 2 — download and verify

Run the one command for the OS as written, without editing it. It downloads `SHA256SUMS` and its
signature `SHA256SUMS.sig` from
`https://github.com/Padelapp-Club/court-gateway/releases/latest/download/` into a private folder
(Linux/macOS: a fresh `mktemp -d`; Windows: the closed `C:\court-gateway`), checks the signature
with `ssh-keygen` against the release key written in the command (never take the key from the
release page), takes the file name for the platform from the verified `SHA256SUMS`, downloads that
file and puts it in place as `court-gateway` (so it can be run) **only** if its checksum matches.
Every failure stops the command with its own message, and nothing is put in place or run.

Linux and macOS:

```sh
(
  fail() { printf '%s\n' "$*" >&2; exit 1; }
  B=https://github.com/Padelapp-Club/court-gateway/releases/latest/download
  D="$PWD"
  case "$(uname -s) $(uname -m)" in
    'Linux x86_64') P=linux-amd64 ;;
    'Linux aarch64' | 'Linux arm64') P=linux-arm64 ;;
    'Darwin arm64') P=darwin-arm64 ;;
    'Darwin x86_64') P=darwin-amd64 ;;
    *) fail "UNSUPPORTED PLATFORM: $(uname -s) $(uname -m)" ;;
  esac
  [ ! -L "$D" ] && [ -O "$D" ] && chmod 700 "$D" || fail "FOLDER NOT OURS: $D"
  T="$(mktemp -d)" && cd "$T" || fail "NO TEMPORARY FOLDER"
  trap 'rm -rf "$T"' EXIT
  curl -fLO "$B/SHA256SUMS" && curl -fLO "$B/SHA256SUMS.sig" || fail "DOWNLOAD FAILED: SHA256SUMS, SHA256SUMS.sig"
  command -v ssh-keygen >/dev/null || fail "NO SSH-KEYGEN: install OpenSSH 8.1 or newer"
  echo 'court-gateway ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJAe4CDRNNnk++UtmLOdDcQcRc0jK56eYJPo+DfdyK8y' > allowed_signers || fail "CANNOT WRITE THE RELEASE KEY: $T"
  S="$(cat SHA256SUMS)" || fail "CANNOT READ SHA256SUMS"
  V="$(printf '%s\n' "$S" | ssh-keygen -Y verify -f allowed_signers -I court-gateway -n court-gateway -s SHA256SUMS.sig 2>&1)" || {
    case "$V" in *'illegal option'* | *'unknown option'* | *usage:*) fail "SSH-KEYGEN TOO OLD: OpenSSH 8.1 or newer is needed" ;; esac
    fail "SIGNATURE MISMATCH: $V"
  }
  L="$(printf '%s\n' "$S" | awk 'NF == 2 && $1 ~ /^[0-9a-f]+$/ && length($1) == 64 && $2 ~ /^court-gateway-[0-9][0-9A-Za-z.-]*-'"$P"'$/ { n++; l = $0 } END { if (n == 1) print l }')"
  [ -n "$L" ] || fail "NOT ONE BUILD FOR $P IN SHA256SUMS"
  F="${L##* }"
  curl -fL -o court-gateway "$B/$F" || fail "DOWNLOAD FAILED: $F"
  printf '%s  court-gateway\n' "${L%% *}" | if command -v sha256sum >/dev/null; then sha256sum -c -; else shasum -a 256 -c -; fi ||
    fail "CHECKSUM MISMATCH: $F"
  chmod 755 court-gateway && mv -f court-gateway "$D/court-gateway" || fail "CANNOT INSTALL THE BINARY INTO $D"
  echo checksum-ok
)
```

Windows: run in PowerShell as Administrator, from any folder (it forces TLS 1.2, which Windows
PowerShell 5.1 does not use by default). `ssh-keygen -Y` needs OpenSSH 8.1 or newer; Windows
10 may have an older one or none, and the command then stops with `NO SSH-KEYGEN` or
`SSH-KEYGEN TOO OLD`: tell the human, run `winget install Microsoft.OpenSSH.Beta`, open a new
PowerShell as Administrator and run it again. Verified on Windows 10 Pro 19045 x64: its built-in
`OpenSSH_for_Windows_9.5` is enough, nothing to install there.

```powershell
& {
  $ProgressPreference = 'SilentlyContinue'
  [Net.ServicePointManager]::SecurityProtocol = 'Tls12'
  $b = 'https://github.com/Padelapp-Club/court-gateway/releases/latest/download'
  $d = 'C:\court-gateway'
  $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
  $a = @{ AMD64 = 'amd64'; ARM64 = 'arm64' }[$cpu]
  if (-not $a) { throw "UNSUPPORTED PLATFORM: $cpu" }
  $ok = 'S-1-5-32-544', 'S-1-5-18', [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  $closed = {
    $acl = Get-Acl -LiteralPath $d -ErrorAction Stop
    $acl.AreAccessRulesProtected -and $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -in $ok -and
      -not @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]) | Where-Object { $_.AccessControlType -ne 'Allow' -or $_.IdentityReference.Value -notin $ok[0..1] })
  }
  if (Test-Path -LiteralPath $d) {
    if (((Get-Item -LiteralPath $d -Force -ErrorAction Stop).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw "FOLDER IS A LINK: C:\court-gateway" }
    $extra = @(Get-ChildItem -LiteralPath $d -Force -Name -ErrorAction Stop | Where-Object { $_ -notmatch '^(allowed_signers|(SHA256SUMS(\.sig)?|court-gateway-[0-9][0-9A-Za-z.\-]*-windows-(amd64|arm64)\.exe)(\.txt|\.part)?)$' })
    if ($extra -and -not (& $closed)) { throw "FOREIGN FILES IN AN OPEN FOLDER: $($extra -join ', ')" }
  } else { New-Item -ItemType Directory -Path $d -ErrorAction Stop | Out-Null }
  icacls $d /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' | Out-Null
  if ($LASTEXITCODE -ne 0 -or -not (& $closed)) { throw "FOLDER NOT CLOSED: C:\court-gateway" }
  Set-Location -LiteralPath $d -ErrorAction Stop
  $drop = { param($p) if (Test-Path -LiteralPath $p) { Remove-Item -LiteralPath $p -Force -ErrorAction Stop } }
  $get = {
    param($n)
    & $drop "$n.part"
    try { Invoke-WebRequest "$b/$n" -OutFile "$n.part" -UseBasicParsing -ErrorAction Stop } catch { throw "DOWNLOAD FAILED: $n ($($_.Exception.Message))" }
    Move-Item -LiteralPath "$n.part" -Destination $n -Force -ErrorAction Stop
  }
  & $get SHA256SUMS
  & $get SHA256SUMS.sig
  if (-not (Get-Command ssh-keygen -ErrorAction SilentlyContinue)) { throw "NO SSH-KEYGEN: install OpenSSH 8.1 or newer" }
  & $drop allowed_signers
  'court-gateway ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJAe4CDRNNnk++UtmLOdDcQcRc0jK56eYJPo+DfdyK8y' | Set-Content -LiteralPath allowed_signers -Encoding ascii -ErrorAction Stop
  $out = cmd /c 'ssh-keygen -Y verify -f allowed_signers -I court-gateway -n court-gateway -s SHA256SUMS.sig < SHA256SUMS 2>&1'
  if ($LASTEXITCODE -ne 0 -and "$out" -match 'unknown option|illegal option|usage:') { throw "SSH-KEYGEN TOO OLD: OpenSSH 8.1 or newer is needed" }
  if ($LASTEXITCODE -ne 0) { throw "SIGNATURE MISMATCH: $out" }
  $m = @(Select-String -LiteralPath SHA256SUMS -Pattern "^([0-9a-f]{64})  (court-gateway-[0-9][0-9A-Za-z.\-]*-windows-$a\.exe)$" -ErrorAction Stop)
  if ($m.Count -ne 1) { throw "NOT ONE BUILD FOR windows-$a IN SHA256SUMS" }
  $want, $f = $m[0].Matches[0].Groups[1].Value, $m[0].Matches[0].Groups[2].Value
  & $get $f
  if ((Get-FileHash -LiteralPath $f -Algorithm SHA256 -ErrorAction Stop).Hash -ne $want) { throw "CHECKSUM MISMATCH: $f" }
  try { Move-Item -LiteralPath $f -Destination court-gateway.exe -Force -ErrorAction Stop } catch { throw "CANNOT REPLACE court-gateway.exe: $($_.Exception.Message)" }
  'checksum-ok'
}
```

Anything but `checksum-ok` (`UNSUPPORTED PLATFORM`, `FOLDER NOT OURS`, `FOLDER IS A LINK`,
`FOREIGN FILES IN AN OPEN FOLDER`, `FOLDER NOT CLOSED`, `DOWNLOAD FAILED`, `NO SSH-KEYGEN`,
`CANNOT WRITE THE RELEASE KEY`, `SSH-KEYGEN TOO OLD`, `SIGNATURE MISMATCH`, `NOT ONE BUILD`,
`CHECKSUM MISMATCH`, `CANNOT INSTALL`, `CANNOT REPLACE`, a `FAILED` line, no output): **stop**,
delete the downloaded files, never run them, and report the message to the human.

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
`GATEWAY_HUB_KEY_FINGERPRINT` are read from the environment; `--crm viva` is the default. Windows: run
it in **PowerShell as Administrator**, `.\court-gateway.exe`, `--config C:\court-gateway\gateway.yaml`, line
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
| `HUB_REDIRECTED` | the hub URL redirects elsewhere and setup follows no redirect — ask the human for the hub's exact URL |
| `HUB_KEY_MISMATCH` | the hub answered with another key than the release's or the fingerprint the human gave, or with an answer that key did not sign — **stop**: the connection may be intercepted; report it, have the human re-check the fingerprint with the operator; another key is found before the join code is spent, an unsigned answer after (the message says which: then a new code is needed) |
| `HUB_RATE_LIMITED` | too many wrong codes from this address — wait a minute, then retry once with a code the human re-checks |
| `CONFIG_EXISTS` | a previous install exists — ask the human before re-running with `--force` |
| `FOLDER_NOT_RESTRICTED` | Windows: the folder could not be closed to everyone but Administrators and SYSTEM — re-run setup in PowerShell as Administrator; nothing was written and the join code is **not** spent |
| `WRITE_FAILED` | `gateway.yaml` or `.env` could not be written (the message names the file) — fix the folder's permissions or free disk space; if the code was already spent, ask for a new one |
| `INTERNAL` | a bug: the config setup built does not load — stop and report the message to the human |

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
| `HUB_AUTH` | the hub does not accept this gateway — a new join code and `setup --force` are needed (the running service restarts by itself on the rewritten files) |
| `TUNNEL_CONNECTED` | the service is not running or not connected yet — check step 4, wait a minute |
| `HUB_CONFIG` | `fail`: the clubs configured on the hub do not load with this CRM — report to the operator (the service keeps the previous config); `warn`: the service has not applied the newest config yet, or a club is served but its config disagrees with the CRM (its time zone) — report to the operator |
| `SECRETS_FILE` | Windows `fail`: the folder holding `.env`, or `.env`, is open to other users — as the `hint` says, in PowerShell as Administrator run `uninstall-service`, then `install-service` (which closes both; on an installed service `install-service` alone fails); `warn` elsewhere: `chmod 600 .env` |
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
