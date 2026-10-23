# macOS packaging

## Layout (`crispy-companion-macos-{arm64,amd64}.tar.gz`)

Each tarball carries the same universal companion binary; the engine differs
by arch (ship per-arch tarballs OR pick at install):

```
crispy-companion               # universal binary (arm64 + x86_64 via lipo)
torrserver                     # pinned MatriX.146.1 darwin artefact for this arch
config.toml                    # seed: loopback defaults, empty crispyServer.url (fail-closed; copy into the data dir to take effect)
README.md                      # this file
```

CI (.github/workflows/macos.yml) builds and checksums both tarballs on the
macOS runners, smoke-runs the daemon to prove the LaunchAgent is written,
and attaches them to tagged releases. The same steps by hand:

```sh
GOOS=darwin GOARCH=arm64 go build -trimpath -o crispy-companion-arm64 ./cmd/crispy-companion
GOOS=darwin GOARCH=amd64 go build -trimpath -o crispy-companion-amd64 ./cmd/crispy-companion
lipo -create -output crispy-companion crispy-companion-arm64 crispy-companion-amd64
```

## Autostart (LaunchAgent, user scope)

Automatic: on the first start the daemon writes
`~/Library/LaunchAgents/io.crispy.Companion.plist` itself and loads it with
`launchctl bootstrap gui/$UID` (falling back to `launchctl load -w`), then
records the outcome in `<data dir>/autostart.json`. The user is not asked —
macOS has no portal dialog to ask one, unlike the Linux Flatpak.

The written agent mirrors `packaging/macos/io.crispy.Companion.plist`:

- launches `crispy-companion --autostart-run` — the flag tells the daemon
  it was started by the OS, so it serves without registering again;
- `RunAtLoad` + `KeepAlive`, so it starts at login and restarts if it dies;
- `WorkingDirectory` = the folder holding the binary, so the `torrserver`
  shipped next to it is found even though launchd starts agents with CWD
  `/`;
- logs to `~/Library/Logs/Crispy Companion/`.

The plist in this directory is the reference shape (its log paths carry a
`/Users/YOU` placeholder) and stays useful for a manual install or when
scripting one.

```sh
# exactly what the daemon does, by hand:
cp packaging/macos/io.crispy.Companion.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/io.crispy.Companion.plist
```

Stopped with `launchctl bootout gui/$UID/io.crispy.Companion`. Re-arrange
after a manual removal with `crispy-companion --request-autostart`. No root
daemon.

## Notifications

Same state as Windows: log fallback today, `UNUserNotificationCenter`
bridge (tiny cgo/objc shim) or `osascript` fallback tracked post-M4.
Pairing and revoke flow through the web app regardless.

## Data dir

`~/Library/Application Support/Crispy Companion` (0600 stores, user-only).

## Notarization

Reserved: `notarytool` submit on tagged releases before any
Homebrew/distribution claim. Unsigned dev builds warn at first run —
by design, never silently bypassed.

## Status

Tarballs build in CI (.github/workflows/macos.yml), which also smoke-runs
the daemon to prove the LaunchAgent and autostart.json are written. First
acceptance: pair against staging on Apple Silicon + Intel; notification
bridge filed as post-M4.
