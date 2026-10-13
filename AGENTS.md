# Installing court-gateway — instructions for AI agents

You are installing court-gateway for a sports club on a machine you can run commands on. Follow
these steps in order. After every step run its **Verify** command and compare the result with
**Expect**. If a step fails twice, stop and report the step, the command and the error code to the
human. Do not improvise around a failure.

## Rules (non-negotiable)

1. **Ask the human, do not guess**, for exactly these inputs:
   - hub URL (comes with the join code);
   - join code (one-time, format `XXXX-XXXX-XXXX-XXXX`);
   - Viva administrator login (e-mail) and **password**;
   - Viva studio id (UUID);
   - the club's time zone if it is not `Europe/Moscow`.
2. **Never echo, print, log or store the Viva password or the join code** — not in your messages,
   not in files other than the `.env` that `setup` writes, not in shell history. Pass them only as
   environment variables of the single `setup` command, or the password via stdin
   (`--password-stdin`). Do not run commands with `set -x`. Never `cat` the `.env` file.
3. **Never open inbound access.** Do not add firewall rules, port forwarding, a public IP, DNS
   records, TLS certificates, reverse proxies or tunnels of your own. Do not add `listen:` to
   `gateway.yaml`. The gateway only connects **out** to the hub over HTTPS.
4. Do not disable automatic updates, do not edit the service files `install-service` writes, do not
   commit or upload `gateway.yaml` or `.env` anywhere.
5. Use the official release binary and verify its checksum (step 2). Build from source only if the
   human asks.

**Success** = `court-gateway doctor --json` exits `0` with every check `ok` (a `warn` on
`VERSION_CURRENT` or `SECRETS_FILE` is acceptable) **and** the operator confirms the hub shows
the club as connected (or `TUNNEL_CONNECTED` is `ok`, which is the same fact seen from the hub).

## Step 0 — collect inputs

Ask the human for the inputs in rule 1. Best: let the human type the secrets into the terminal
you share, without echo (works in bash and zsh):

```sh
printf 'Hub URL: ';            read -r GATEWAY_HUB_URL
printf 'Join code: ';          stty -echo; read -r GATEWAY_JOIN_CODE; stty echo; echo
printf 'Viva login (e-mail): '; read -r CRM_USERNAME
printf 'Viva password: ';      stty -echo; read -r CRM_PASSWORD; stty echo; echo
printf 'Viva studio id: ';     read -r CRM_STUDIO
export GATEWAY_HUB_URL GATEWAY_JOIN_CODE CRM_USERNAME CRM_PASSWORD CRM_STUDIO
```

```powershell
# Windows PowerShell (run as Administrator)
$env:GATEWAY_HUB_URL   = Read-Host 'Hub URL'
$env:GATEWAY_JOIN_CODE = [Net.NetworkCredential]::new('', (Read-Host 'Join code' -AsSecureString)).Password
$env:CRM_USERNAME      = Read-Host 'Viva login (e-mail)'
$env:CRM_PASSWORD      = [Net.NetworkCredential]::new('', (Read-Host 'Viva password' -AsSecureString)).Password
$env:CRM_STUDIO        = Read-Host 'Viva studio id'
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

Detect: `uname -s -m` (Linux/macOS) or `$env:PROCESSOR_ARCHITECTURE` (Windows). Create the folder
and `cd` into it. On Windows restrict it first:
`icacls C:\court-gateway /inheritance:r /grant:r "Administrators:(OI)(CI)F" "SYSTEM:(OI)(CI)F"`.

**Verify:** `pwd` / `Get-Location`. **Expect:** the folder from the table.

## Step 2 — download and verify

Release page: `https://github.com/Padelapp-Club/court-gateway/releases/latest/download/<file>`.
Download `SHA256SUMS` and the release file for the platform, then:

```sh
sha256sum -c --ignore-missing SHA256SUMS        # Linux
shasum -a 256 -c --ignore-missing SHA256SUMS    # macOS
mv court-gateway-*-* court-gateway && chmod +x court-gateway
```

```powershell
$f = Get-ChildItem court-gateway-*-windows-amd64.exe
$want = (Select-String -Path SHA256SUMS -Pattern $f.Name).Line.Split(' ')[0]
if ((Get-FileHash $f -Algorithm SHA256).Hash -ieq $want) { 'checksum-ok' } else { 'CHECKSUM MISMATCH' }
Rename-Item $f court-gateway.exe
```

On macOS, if the binary is quarantined: `xattr -d com.apple.quarantine court-gateway`.

**Verify:** `./court-gateway version` (Windows: `.\court-gateway.exe version`).
**Expect:** `court-gateway <v> (release, <os>-<arch>)`. `selfbuilt` instead of `release` means a
binary without the release key: it works but never updates itself — tell the human.

## Step 3 — setup

```sh
./court-gateway setup --non-interactive --json \
  --hub-url "$GATEWAY_HUB_URL" --crm-username "$CRM_USERNAME" --crm-studio "$CRM_STUDIO" \
  --config "$PWD/gateway.yaml" > setup-result.json
```

(`CRM_PASSWORD` and `GATEWAY_JOIN_CODE` are read from the environment. Add
`--timezone <Area/City>` if the club is not in `Europe/Moscow`. Windows: `.\court-gateway.exe`,
`--config C:\court-gateway\gateway.yaml`, line continuation with a backtick.)

Then remove the secrets from your shell: `unset CRM_PASSWORD GATEWAY_JOIN_CODE`
(PowerShell: `Remove-Item Env:CRM_PASSWORD, Env:GATEWAY_JOIN_CODE`).

**Verify:** read `setup-result.json` (it never contains secrets).
**Expect:** `"ok": true`. Then show the human, in plain words: the club name, the list of
`courts`, `hours`, and every entry of `warnings` — especially `COURTS_FROM_BOOKINGS` (a court with
no bookings in the next 28 days is missing and must be added by hand) and `PRICE_LIST_AMBIGUOUS`.
Edit `gateway.yaml` only as the human asks.

If `"ok": false`, act on `error.code`:

| Code | Action |
|---|---|
| `MISSING_INPUT` | an input from step 0 is empty — ask the human again |
| `CRM_AUTH_FAILED` | wrong Viva login or password — ask the human to re-enter; the join code is **not** spent |
| `CRM_REJECTED` | wrong studio id — ask the human |
| `CRM_UNREACHABLE` | no internet or Viva is down — check `curl -sI https://api.vivacrm.ru`, retry later |
| `NO_COURTS_FOUND` | no bookings in 28 days — confirm the studio id with the human |
| `JOIN_CODE_REJECTED` | code wrong, used or expired — ask the human for a new code |
| `HUB_UNREACHABLE` | check the hub URL and outbound HTTPS; never open inbound ports |
| `CLUB_MISMATCH` | the code belongs to another club — ask the human for the right code |
| `CONFIG_EXISTS` | a previous install exists — ask the human before re-running with `--force` |

## Step 4 — install the service

| OS | Command | Verify | Expect |
|---|---|---|---|
| Linux | `sudo ./court-gateway install-service` | `systemctl is-active court-gateway` | `active` |
| macOS | `sudo ./court-gateway install-service` | `sudo launchctl print system/court-gateway \| grep state` | `state = running` |
| Windows | `.\court-gateway.exe install-service` (Administrator) | `sc.exe query court-gateway` | `STATE : 4 RUNNING` |

On a cloud VM without a regular user, add `--run-as <user>` with a dedicated user that owns the
folder (the folder must stay writable by that user so updates can be installed).

## Step 5 — doctor

Wait 30 seconds, then:

```sh
./court-gateway doctor --json -config "$PWD/gateway.yaml"; echo "exit=$?"
```

**Expect:** `exit=0`, `"ok": true`, and these checks `ok`: `CONFIG_VALID`, `CRM_LOGIN`,
`CRM_OCCUPANCY`, `HUB_REACHABLE`, `HUB_AUTH`, `TUNNEL_CONNECTED`. For any `fail`, follow its `hint`;
the codes are stable:

| Code | Meaning when failing |
|---|---|
| `CONFIG_VALID` | `gateway.yaml` or `.env` is broken — the message lists every problem |
| `CRM_LOGIN` / `CRM_OCCUPANCY` | the Viva login or the studio/rooms are wrong, or Viva is unreachable |
| `HUB_REACHABLE` | no outbound HTTPS to the hub, or its certificate failed verification |
| `HUB_AUTH` | the hub does not accept this gateway — a new join code and `setup --force` are needed |
| `TUNNEL_CONNECTED` | the service is not running or not connected yet — check step 4, wait a minute |
| `VERSION_CURRENT` | `warn` only: self-built binary, or an update is pending (installs itself) |
| `LOCAL_PORT` | `warn`: `listen:` is set — remove it unless the human asked for it |

## Step 6 — report

Tell the human: installed version, club id, number of courts, the warnings they still have to act
on, where the log is (Linux `journalctl -u court-gateway`, macOS/Windows `court-gateway.log` next
to `gateway.yaml`), and that the operator can confirm the club is connected in the hub. Do not
include any secret in the report.

## Uninstall

`sudo ./court-gateway uninstall-service` (Windows: `.\court-gateway.exe uninstall-service`), then
delete the folder.
