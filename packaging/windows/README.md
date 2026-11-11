# Windows packaging

## Layout

The installer (`crispy-companion.nsi`, built by `.github/workflows/windows.yml`
on a `windows-latest` runner with `makensis`) is per-user: `RequestExecutionLevel
user`, no UAC, no admin, nothing outside the user's own profile.

```
%LOCALAPPDATA%\Programs\Crispy Companion\    install dir
    crispy-companion.exe    the daemon (Go, no cgo)
    torrserver.exe          pinned MatriX.146.1 artefact, sha256-checked in CI
    config.toml             seed, copied into the data dir on first install
    crispy-companion.ico    the same icon the macOS and Linux builds ship
    LICENSE.txt             GPL-3.0-only
    uninstall.exe
%LOCALAPPDATA%\Crispy Companion\             data dir
    config.toml             the config the daemon actually reads
    tokens.json             device tokens (0600)
    device-session.json
    pairing-state.json      pending pairing mirror for `pair show`
    autostart.json          outcome of the autostart registration
    cache/torrserver/       engine cache
Start Menu\Programs\Crispy Companion.lnk     --menu-launch entry
Settings -> Apps -> Installed apps           the uninstall entry
```

The install dir is remembered per user (`HKCU\Software\Crispy Companion`,
`InstallDir`), so an upgrade lands where the previous install is — which
matters, because the autostart value below points into it.

`config.toml` lives in the data dir, not next to the binary: the daemon reads
it from there (`CRISPY_COMPANION_CONFIG` overrides the path). The installer
seeds a copy once, on first install only, and never overwrites an edited one.
`crispyServer.url` stays empty: pairing is disabled until it is set here, in
`CRISPY_SERVER_URL`, or through a build that carries deployment defaults.

Build by hand — staging goes into `packaging/windows/stage`, the .nsi's
default `STAGEDIR`. The `-NOCD` switch matters: makensis otherwise changes
its working directory to the script's folder, and the script's payload,
icon and `LICENSE` paths are relative to the repository root:

```powershell
$env:CGO_ENABLED = "0"
go build -trimpath -o packaging/windows/stage/crispy-companion.exe ./cmd/crispy-companion
Copy-Item packaging/windows/config.toml, packaging/windows/icons\crispy-companion.ico `
          -Destination packaging/windows/stage
makensis /NOCD -DOUTFILE=crispy-companion-setup.exe packaging/windows/crispy-companion.nsi
```

The engine is a third-party artefact, not built here: fetch the pinned one
from the TorrServer release and drop it into the stage as `torrserver.exe`.
The release workflow does exactly that, with a sha256 check.

## No-installer zip

`crispy-companion-windows-amd64.zip` is flat and carries the install dir's
runnable files for a folder install (USB stick, locked-down machine, "just
let me run it"): `crispy-companion.exe`, `torrserver.exe`, `config.toml`,
`crispy-companion.ico`, `LICENSE.txt`, `README.md`. Extract it anywhere and
run `crispy-companion.exe`; the daemon works the same way — the engine is
found next to the exe, and a `config.toml` dropped into
`%LOCALAPPDATA%\Crispy Companion` still applies. The zip is what the release
workflow attaches next to the setup.

## Autostart (user scope only)

Not written by the installer: on the first start the daemon writes the Run
value itself (`reg add`, so no registry binding or extra dependency),
pointing at its own executable plus `--autostart-run`, and records the
outcome in `%LOCALAPPDATA%\Crispy Companion\autostart.json`. The flag tells
the daemon it was started by Windows, so it serves without registering
again. Windows has no portal dialog to ask the user, unlike the Linux
Flatpak.

What it writes:

```cmd
reg add HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v "Crispy Companion" /t REG_SZ /d "\"%LOCALAPPDATA%\Programs\Crispy Companion\crispy-companion.exe\" --autostart-run" /f
```

`HKCU`, never `HKLM`; the value shows up in Task Manager -> Startup apps and
can be disabled there. `reg delete ... /v "Crispy Companion"` removes it by
hand, the uninstaller removes it too, and `crispy-companion.exe
--request-autostart` re-arranges it after a manual removal.

Since the Run value has no working directory, the engine is found by the
same-name-next-to-the-binary rule (see the engine supervisor): the
`torrserver.exe` shipped beside `crispy-companion.exe` is picked up even
though Windows starts the value from system32.

## Notifications

M2's portal notifier is Linux-only; on Windows the companion currently falls
back to log lines. Toasts with a Revoke action need a Start Menu entry that
carries an AppUserModelID, which the installer creates but does not set yet —
tracked work. Until then `pair show` prints the pending pairing, and revoke
runs from Crispy settings -> Devices.

## Data dir

`%LOCALAPPDATA%\Crispy Companion`, ACLs: current user only. The uninstaller
removes it — the device pairing and the engine cache go with it.

## Signing

Unsigned for now: SmartScreen shows "Windows protected your PC" on first run
and users click through. Both the installer and the daemon carry version
resources (the daemon's via `-X ...internal/server.Version`). Azure Artifact
Signing (formerly Trusted Signing) is the CI-friendly path to a signature when
that becomes worth ~$10/month.

## Status

Installer and CI (.github/workflows/windows.yml) build on every push/PR and
attach the setup + zip to tagged releases. First acceptance: install on a
Windows host, pair against the staging server, confirm the Start Menu entry
opens the web app and the Run value starts the daemon at login. Toasts filed
as post-M4.
