# macOS packaging (M4 draft — not yet built)

## Layout (`crispy-companion-macos-universal.tar.gz`)

```
crispy-companion               # universal binary (arm64 + x86_64 via lipo)
torrserver                     # pinned MatriX.146.1 darwin artefact per arch (ship per-arch zips OR pick at install)
config.toml                    # seed: loopback defaults, empty crispyServer.url (fail-closed)
```

```sh
GOOS=darwin GOARCH=arm64 go build -trimpath -o crispy-companion-arm64 ./cmd/crispy-companion
GOOS=darwin GOARCH=amd64 go build -trimpath -o crispy-companion-amd64 ./cmd/crispy-companion
lipo -create -output crispy-companion crispy-companion-arm64 crispy-companion-amd64
```

## Autostart (LaunchAgent, user scope)

`packaging/macos/io.crispy.Companion.plist` → `~/Library/LaunchAgents/`:

```sh
cp packaging/macos/io.crispy.Companion.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/io.crispy.Companion.plist
```

`KeepAlive=true`, stoppable via `launchctl unload`. No root daemon.

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

Draft. First acceptance: tarball on Apple Silicon + Intel, pair against
staging, notification bridge filed as post-M4.
