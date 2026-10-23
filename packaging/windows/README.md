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

Automatic: on the first start the daemon writes the Run value itself
(`reg add`, so no registry binding or extra dependency), pointing at its
own executable plus `--autostart-run`, and records the outcome in
`%LOCALAPPDATA%\Crispy Companion\autostart.json`. The flag tells the daemon
it was started by Windows, so it serves without registering again. Windows
has no portal dialog to ask the user, unlike the Linux Flatpak.

What it writes:

```cmd
reg add HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v "Crispy Companion" /t REG_SZ /d "\"%LOCALAPPDATA%\Crispy Companion\crispy-companion.exe\" --autostart-run" /f
```

`HKCU`, never `HKLM`; the value shows up in Task Manager → Startup apps and
can be disabled there, and `reg delete ... /v "Crispy Companion"` removes
it. Re-arrange after a manual removal with `crispy-companion.exe
--request-autostart`. A future MSI runs this as a per-user checkbox.

Since the Run value has no working directory, the engine is found by the
same-name-next-to-the-binary rule (see the engine supervisor): the
`torrserver.exe` shipped beside `crispy-companion.exe` is picked up even
though Windows starts the value from system32.

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
