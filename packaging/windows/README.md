# Windows packaging (M4 draft — not yet built)

## Layout (`crispy-companion-windows-amd64.zip`)

```
crispy-companion.exe          # shim (embeds nothing; see TorrServer note)
torrserver.exe                # pinned MatriX.146.1 artefact, sha256-checked at release time
config.toml                   # seed: loopback defaults, empty crispyServer.url (fail-closed)
README.txt                    # pair flow: settings → Pair → approve in web app
```

Built on a Windows host (or `GOOS=windows go build`; the `.exe` needs no
cgo):

```sh
GOOS=windows GOARCH=amd64 go build -trimpath -o crispy-companion.exe ./cmd/crispy-companion
```

## Autostart (user scope only)

```cmd
reg add HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v "Crispy Companion" /t REG_SZ /d "\"%LOCALAPPDATA%\Crispy Companion\crispy-companion.exe\"" /f
```

`HKCU`, never `HKLM`. A future MSI runs this as a per-user checkbox.

## Notifications

M2's portal notifier is Linux-only; on Windows the companion currently
falls back to log lines. Toasts with a Revoke action (via a
`windows-toast-notifications` family lib) are tracked work — until then
`pair show` equivalent: poll `GET /companion/status?pendingPairId=` from
the browser (the normal web flow needs no CLI), revoke from Crispy
settings → Devices.

## Data dir

`%LOCALAPPDATA%\Crispy Companion` (`tokens.json`, `device-session.json`,
`pairing-state.json`, engine cache). ACLs: current user only.

## Status

Draft. First acceptance: zip on a Windows host, pair against staging
server, toast follow-up filed as post-M4.
