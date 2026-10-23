# Linux packaging (non-Flatpak)

For distro packages and tarball installs. The Flatpak bundle gets its
login autostart from the desktop's Background portal (see
`../flatpak/README.md`); a plain install has no portal request to make,
so it ships an XDG autostart entry instead — the same pattern Pika
Backup uses for its monitor daemon.

## Autostart (XDG, system scope)

`packaging/linux/io.crispy.Companion.autostart.desktop` → `/etc/xdg/autostart/`:

```sh
install -Dm644 packaging/linux/io.crispy.Companion.autostart.desktop \
  /etc/xdg/autostart/io.crispy.Companion.desktop
```

- `NoDisplay=true` keeps the entry out of every menu — it is
  autostart-only.
- `Exec=crispy-companion --autostart-run` must resolve on the user's
  PATH; distro packages install the binary to `/usr/bin`.
  `--autostart-run` tells the daemon it was started by the desktop's own
  autostart entry, so it serves without asking the Background portal
  again (no dialog at every login).
- `X-GNOME-Autostart-Delay=90` defers the start so login is not slowed
  down (honoured by GNOME and KDE).
- `X-GNOME-UsesNotifications=true` marks the process as a notification
  source, so the shell does not treat it as a stray process to kill.

Per-user install instead of system scope:

```sh
install -Dm644 packaging/linux/io.crispy.Companion.autostart.desktop \
  ~/.config/autostart/io.crispy.Companion.desktop
```

Remove the file to disable. The daemon is idempotent: a second start
finds the running instance and exits with an "already running"
notification, so a leftover autostart entry can never spawn two
engines.

## Permissions

The daemon binds a loopback port (>1024) and needs no privileges.
`shared.mode = "raw"` additionally binds the configured LAN address and
serves plaintext HTTP — that mode needs the host firewall opened
manually and is never enabled automatically.
