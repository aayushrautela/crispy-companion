// Command crispy-companion is the headless Crispy companion daemon.
//
// M1: loopback listener with perimeter middleware, engine supervisor,
// file-backed config + token store. No GUI; logs go to stderr (Flatpak
// captures them via journal).
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/autostart"
	"github.com/crispy-streaming/crispy-companion/internal/browser"
	"github.com/crispy-streaming/crispy-companion/internal/config"
	"github.com/crispy-streaming/crispy-companion/internal/engine"
	"github.com/crispy-streaming/crispy-companion/internal/grant"
	"github.com/crispy-streaming/crispy-companion/internal/httpx"
	"github.com/crispy-streaming/crispy-companion/internal/notify"
	"github.com/crispy-streaming/crispy-companion/internal/pairing"
	"github.com/crispy-streaming/crispy-companion/internal/portal"
	"github.com/crispy-streaming/crispy-companion/internal/server"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("crispy-companion: %v", err)
	}
}

func run() error {
	// Headless pairing helper (no daemon needed): prints the pending
	// pairing's code + verification URL from the state file the daemon
	// mirrors on every /pair.
	if len(os.Args) >= 3 && os.Args[1] == "pair" && os.Args[2] == "show" {
		return pairShow(resolveDataDir())
	}
	fs := flag.NewFlagSet("crispy-companion", flag.ContinueOnError)
	requestAutostart := fs.Bool("request-autostart", false, "force a login-autostart request (normally arranged once on first start), then run")
	autostartRun := fs.Bool("autostart-run", false, "started by the login-autostart entry: serve, but do not arrange autostart again")
	menuLaunch := fs.Bool("menu-launch", false, "started from the desktop start-menu entry: report the outcome as a notification")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	return runDaemon(*requestAutostart, *autostartRun, *menuLaunch)
}

// resolveDataDir applies the CRISPY_COMPANION_DATA_DIR override.
func resolveDataDir() string {
	dataDir := defaultDataDir()
	if v := os.Getenv("CRISPY_COMPANION_DATA_DIR"); v != "" {
		dataDir = v
	}
	return dataDir
}

// pairShow prints the pending pairing for portal-less setups. Approval
// itself always happens in the logged-in webapp; this only displays where
// to approve.
func pairShow(dataDir string) error {
	st, err := pairing.ReadPairState(filepath.Join(dataDir, "pairing-state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no pairing pending")
			return nil
		}
		return err
	}
	if time.Now().After(st.ExpiresAt) {
		pairing.RemovePairState(filepath.Join(dataDir, "pairing-state.json"))
		fmt.Println("pending pairing expired — start a new one from Crispy settings")
		return nil
	}
	fmt.Printf("user code:        %s\napprove at:       %s\nexpires:         %s\n", st.UserCode, st.VerificationURL, st.ExpiresAt.Local().Format(time.RFC1123))
	return nil
}

func runDaemon(requestAutostart, autostartRun, menuLaunch bool) error {
	logger := log.New(os.Stderr, "crispy-companion: ", log.LstdFlags)

	dataDir := resolveDataDir()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	configPath := os.Getenv("CRISPY_COMPANION_CONFIG")
	if configPath == "" {
		configPath = filepath.Join(dataDir, "config.toml")
	}
	cfg, err := config.Load(dataDir, configPath)
	if err != nil {
		return err
	}

	// SharedAddr starts empty (loopback-only) and is filled when an
	// off-loopback listener comes up. Handlers read it per-request.
	sharedAddr := &stringHolder{}

	// Host allowlist: exact loopback hosts for the configured port.
	port := cfg.ListenPort
	hosts := []string{
		"127.0.0.1:" + strconv.Itoa(port),
		"localhost:" + strconv.Itoa(port),
	}
	dev := os.Getenv("CRISPY_DEV") == "true"
	origins := append([]string(nil), cfg.AllowedOrigins...)
	if dev {
		origins = append(origins, "http://127.0.0.1:5173", "http://localhost:5173")
	}
	policy := httpx.NewPolicy(hosts, origins, dev)

	tokens, err := pairing.Open(filepath.Join(dataDir, "tokens.json"))
	if err != nil {
		return err
	}
	session, err := pairing.OpenSession(filepath.Join(dataDir, "device-session.json"))
	if err != nil {
		return err
	}
	window := pairing.NewWindow(
		time.Duration(cfg.PendingTTLSeconds)*time.Second,
		cfg.MaxAttemptsPerMinute,
	)
	// The server the device grant runs against. Empty by default:
	// new pairings fail closed until configured (PLAN D8).
	grantClient := grant.New(cfg.CrispyServerURL)

	// Prefer portal notifications (with a working Revoke button) when a
	// session bus exists; otherwise log (Flatpak captures stderr). Wired
	// before the engine so the start-menu entry can report "already
	// running" without starting a second engine.
	var notifier notify.Notifier = notify.LogNotifier{Log: logger}
	var pc *portal.Conn
	if c, err := portal.Dial(); err != nil {
		logger.Printf("portal unavailable, notifications go to log: %v", err)
	} else {
		pc = c
		defer pc.Close()
		notifier = notify.NewPortal(pc, func(id, action string) {
			if id != "crispy-pairing" || action != "revoke" {
				return
			}
			logger.Printf("revoke requested from notification")
			_ = tokens.RevokeAll()
			_ = session.Clear()
		})
	}

	// A second instance (start-menu click, manual run) must never die
	// silently: if the companion is already up, say so and exit. Without
	// this the user clicks the icon and nothing at all happens.
	if probeCompanion(cfg.Addr()) {
		logger.Printf("companion already running on %s", cfg.Addr())
		_ = notifier.Post(notify.Notification{
			Title: "Crispy Companion is already running",
			Body:  "The local engine is up. Open the Crispy web app to pair or stream.",
		})
		if menuLaunch {
			openWebApp(cfg.WebAppURL, notifier, logger)
		}
		return nil
	}

	eng, err := engine.New(engine.Config{
		Addr:          cfg.EngineAddr,
		Port:          cfg.EnginePort,
		AutoStart:     cfg.EngineAutoStart,
		BinaryPath:    cfg.EngineBinaryPath,
		CacheDir:      filepath.Join(dataDir, "cache", "torrserver"),
		MaxCacheBytes: cfg.EngineMaxCacheBytes,
	})
	if err != nil {
		return err
	}
	if err := eng.Start(); err != nil {
		return err
	}
	// A dead engine is not fatal (pairing, revocation and status keep
	// working), but torrents cannot play without it, so say why.
	if err := eng.StartError(); err != nil {
		logger.Printf("torrent engine not started: %v (torrent playback will fail until it runs)", err)
	}
	defer eng.Stop()

	mode := "loopback"
	if cfg.SharedMode != "off" {
		mode = cfg.SharedMode
	}
	buffer := server.BufferConfig{
		MinBytes:                 cfg.BufferMinBytes,
		StartSeconds:             cfg.BufferStartSeconds,
		BitrateWindowSeconds:     cfg.BufferBitrateWindowSeconds,
		SustainableMarginPercent: cfg.BufferSustainableMarginPercent,
	}
	deps := server.Deps{
		Buffer:     buffer,
		V1:         server.NewV1State(buffer, false),
		Policy:     policy,
		Tokens:     tokens,
		Window:     window,
		Engine:     eng,
		Notify:     notifier,
		Mode:       mode,
		Grant:      grantClient,
		Session:    session,
		DeviceName: defaultDeviceName(),
		Torrents:   engine.NewClient("http://" + cfg.EngineAddr + ":" + strconv.Itoa(cfg.EnginePort)),
		Links:      engine.NewLinkStore(),
		// Headless mirror for `pair show` (no secrets inside).
		PairingStatePath: filepath.Join(dataDir, "pairing-state.json"),
		SharedAddr:       sharedAddr.Load,
	}
	handler := server.New(deps)

	// Lazy revocation check (PLAN §5.2): every 15 min, revalidate the
	// stored cp_pat_. 401 (revoked/expired) wipes local tokens; 403 means
	// the PAT is alive but not a user session (expected); network errors
	// are ignored so existing tokens keep working offline.
	if grantClient.ServerURL != "" {
		go revalidateLoop(grantClient, session, tokens, notifier, logger)
	}

	// Listener hygiene (PLAN §6): fixed loopback port, no reuse flags.
	// Go's net.Listen never sets SO_REUSEPORT; SO_REUSEADDR semantics on
	// Linux do not permit hijack of a bound port — a second binder fails
	// loudly, which is the desired behavior.
	lc := net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", cfg.Addr())
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			// Authoritative "already running": this instance lost the race
			// for the port. Report it instead of dying silently.
			logger.Printf("port %s already in use: companion already running", cfg.Addr())
			_ = notifier.Post(notify.Notification{
				Title: "Crispy Companion is already running",
				Body:  "The local engine is up. Open the Crispy web app to pair or stream.",
			})
			if menuLaunch {
				openWebApp(cfg.WebAppURL, notifier, logger)
			}
			return nil
		}
		return err
	}
	// Refuse to run if we somehow bound off-loopback.
	if addr, ok := ln.Addr().(*net.TCPAddr); ok && !addr.IP.IsLoopback() {
		_ = ln.Close()
		return errors.New("refusing to serve the primary listener on a non-loopback address")
	}
	logger.Printf("listening on http://%s (mode=%s)", ln.Addr(), mode)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	servers := []*http.Server{srv}
	var serversMu sync.Mutex
	track := func(s *http.Server) {
		serversMu.Lock()
		servers = append(servers, s)
		serversMu.Unlock()
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("server error: %v", err)
		}
	}()

	// A headless daemon has no window, so the start-menu entry has to
	// answer the click with something visible: the notification below,
	// and — when a web app URL is configured — the Crispy web app in
	// the default browser. Unconditional: the click must never look
	// like a no-op, and the flag may not survive the desktop's Exec
	// rewriting.
	if menuLaunch {
		logger.Printf("launched from the start-menu entry")
	}
	_ = notifier.Post(notify.Notification{
		Title: "Crispy Companion started",
		Body:  "Listening on 127.0.0.1:" + strconv.Itoa(port) + ". Open the Crispy web app to pair this device.",
	})
	if menuLaunch {
		openWebApp(cfg.WebAppURL, notifier, logger)
	}

	// M5 shared listeners. Loopback always serves; these add a second
	// surface. The engine stays loopback-only in every mode.
	switch cfg.SharedMode {
	case "raw":
		if err := serveRaw(cfg, port, hosts, origins, dev, deps, sharedAddr, track, logger); err != nil {
			return err
		}
	}

	// Pika Backup-style background handling (best-effort): declare that
	// this process is intentionally running in the background, then
	// arrange login autostart — skipped once granted, re-asked on
	// regular launches after a refusal (desktops answer refusals from
	// their permission store, so a re-enabled app heals its own state).
	// Both happen after the listeners are up so a second instance
	// ("already running" above) never triggers a portal dialog, and no
	// listener waits for the user to answer the dialog.
	//
	// autostartRun means the OS started us from its own autostart entry:
	// autostart is on by definition, so arranging it again would only
	// nag.
	if pc != nil {
		_ = pc.SetStatus("Torrent streaming ready")
	}
	if !autostartRun {
		ensureAutostart(pc, notifier, requestAutostart, dataDir, logger)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serversMu.Lock()
	defer serversMu.Unlock()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
	return nil
}

// openWebApp answers a start-menu click with the Crispy web app in the
// default browser — the headless daemon's only window. Only
// --menu-launch calls it: login autostart (--autostart-run) and plain
// CLI launches keep serving silently exactly as before. Best-effort by
// design: an unset or invalid URL only logs, and a failed opener
// degrades to a notification carrying the URL, so the click always
// answers with something visible.
func openWebApp(rawURL string, notifier notify.Notifier, logger *log.Logger) {
	if rawURL == "" {
		logger.Printf("no web app URL configured: the menu entry answers with the notification only (set webapp.url or CRISPY_COMPANION_WEBAPP_URL)")
		return
	}
	err := browser.Open(rawURL)
	switch {
	case errors.Is(err, browser.ErrInvalidURL):
		logger.Printf("web app URL %q is not an absolute http(s) URL; not opening a browser", rawURL)
	case err != nil:
		logger.Printf("could not open %s in the default browser: %v", rawURL, err)
		_ = notifier.Post(notify.Notification{
			Title: "Crispy Companion is running",
			Body:  "Open " + rawURL + " in a browser to pair or stream.",
		})
	default:
		logger.Printf("opened the Crispy web app (%s) in the default browser", rawURL)
	}
}

// autostartCommand is what the desktop writes into the login-autostart
// entry. It must be the plain serve command: the exported start-menu
// entry carries --menu-launch, and the portal falls back to that Exec
// key when no commandline is passed (which would make the login start
// look like a menu click).
//
// --autostart-run tells the daemon it was started by the desktop's own
// autostart entry, so it serves without asking the portal a second time.
var autostartCommand = []string{"crispy-companion", "--autostart-run"}

// autostartReason is shown to the user in the desktop's dialog.
const autostartReason = "Keep torrent streaming available in the background"

// ensureAutostart arranges login autostart the way the platform expects:
// the desktop portal on Linux (the desktop's own permission state
// decides, and the outcome is recorded either way), the platform's own
// registration elsewhere — a LaunchAgent on macOS, the HKCU Run value on
// Windows, the XDG entry on a Linux host without a portal (no bus, or a
// bus with no portal behind the name).
//
// A granted answer settles the question. A refusal does not: every
// regular launch — never an --autostart-run start made by the desktop
// itself — asks again, which heals the state when the user re-enables
// the app in desktop settings and is otherwise silent, since desktops
// answer refusals straight from their permission store. A failed
// attempt settles nothing — the next start retries. force
// (--request-autostart) always tries again, which is the way back after
// the user revoked autostart in the OS or desktop settings.
//
// Inside a Flatpak the portal is the only path: the sandbox sees its
// own private home, so a direct registration would write an entry the
// desktop never runs. There the portal's absence is reported to the
// user rather than recorded as a success.
//
// dbus-activatable stays false: the companion has no D-Bus activation
// service, and activating through D-Bus would start the app through the
// desktop file rather than the pinned command.
func ensureAutostart(pc *portal.Conn, notifier notify.Notifier, force bool, dataDir string, logger *log.Logger) {
	statePath := autostart.StatePath(dataDir)
	if !force {
		if st, ok := autostart.ReadState(statePath); ok && st.Enabled {
			// Granted — the desktop already runs us at login; asking
			// again per launch would only churn. --request-autostart
			// forces a fresh round.
			return
		}
		// A refusal never settles: the ask below repeats on regular
		// launches and heals the record when the user re-enables us.
	}
	now := time.Now().UTC()
	record := func(st autostart.State) {
		if err := autostart.WriteState(statePath, st); err != nil {
			logger.Printf("could not record the autostart answer: %v", err)
		}
	}
	// Outcomes that need a fix outside this daemon (a refusal in
	// desktop settings, a missing portal package) must reach the user:
	// Pika Backup dialogs them, but this daemon has no window and log
	// lines are journal entries nobody opens. So post a notification
	// on top of logging.
	tell := func(title, body string) {
		_ = notifier.Post(notify.Notification{Title: title, Body: body})
	}

	if pc != nil {
		bg, auto, err := pc.RequestBackground(autostartReason, true, autostartCommand, false, 30*time.Second)
		switch {
		case err == nil && bg && auto:
			logger.Printf("autostart granted: the companion will start at login")
			record(autostart.State{Enabled: true, Method: autostart.MethodPortal, RequestedAt: now})
			return
		case err == nil:
			logger.Printf("background/autostart refused (background=%v autostart=%v); asking again on the next regular launch, --request-autostart forces it", bg, auto)
			record(autostart.State{Refused: true, RequestedAt: now})
			tell("Login autostart is off",
				"Enable Crispy Companion under your desktop's background-app settings to start it at login.")
			return
		case !portal.Unavailable(err):
			// A real answer failed, was cancelled or timed out. No state:
			// the next start asks again. No direct fallback either — on a
			// desktop the entry must come from the user's answer, not
			// around it.
			logger.Printf("autostart request failed: %v", err)
			return
		}
		// The bus is there but no portal owns the name: nobody can ask
		// the user. Fall through to the direct registration below —
		// right on the host, never recorded inside a Flatpak.
		logger.Printf("desktop portal unavailable (%v)", err)
	}

	// No portal answered (no bus, or no portal behind the name). On the
	// host that keeps working: macOS and Windows always land here, and
	// Linux sessions register the XDG entry directly. Inside a Flatpak
	// there is nothing this process can do — the desktop reads
	// autostart entries from the host home and the sandbox sees its
	// own — so surface the misconfiguration instead of recording a
	// "success" that never runs.
	if autostart.InFlatpak() {
		logger.Printf("no desktop portal inside the Flatpak: leaving login autostart unset (retried on the next start)")
		tell("Login autostart could not be arranged",
			"No desktop background portal answered this session (xdg-desktop-portal). The companion retries each start.")
		return
	}

	// No portal at all: macOS and Windows always land here, Linux on
	// hosts whose session has no desktop portal.
	exe, err := os.Executable()
	if err != nil {
		logger.Printf("autostart registration failed: cannot locate the companion binary: %v", err)
		return
	}
	method, err := autostart.Register(exe, logger)
	if err != nil {
		// No state: the next start retries.
		logger.Printf("autostart registration failed: %v", err)
		return
	}
	logger.Printf("autostart registered (%s): the companion will start at login", method)
	record(autostart.State{Enabled: true, Method: method, RequestedAt: now})
}

// serveRaw serves plaintext HTTP on the configured LAN address (validated
// non-loopback by config). Warn loudly: tokens and streams cross the LAN
// segment unencrypted. A bind failure is fatal (fail fast on misconfig).
func serveRaw(cfg config.Config, port int, loopHosts, origins []string, dev bool, deps server.Deps, sharedAddr *stringHolder, track func(*http.Server), logger *log.Logger) error {
	addr := net.JoinHostPort(cfg.SharedListenAddr, strconv.Itoa(port))
	logger.Printf("WARNING: shared.mode=raw serves PLAINTEXT HTTP on %s — tokens and streams are visible on the LAN segment; keep this address on a trusted LAN", addr)
	sdeps := deps
	sdeps.Policy = httpx.NewPolicy(append(append([]string(nil), loopHosts...), addr), origins, dev)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("raw shared listen: %w", err)
	}
	sharedAddr.Store("http://" + ln.Addr().String())
	srv := &http.Server{
		Handler:           server.New(sdeps),
		ReadHeaderTimeout: 5 * time.Second,
	}
	track(srv)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("raw shared server error: %v", err)
		}
	}()
	return nil
}

// probeCompanion reports whether a Crispy companion daemon is already
// serving on addr. It asks the daemon's own /companion/ping endpoint and
// checks the service name, so an unrelated process squatting on the port
// is not mistaken for a running companion.
func probeCompanion(addr string) bool {
	client := &http.Client{Timeout: 750 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/companion/ping")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}
	return resp.StatusCode == http.StatusOK && bytes.Contains(body, []byte(`"name":"crispy-companion"`))
}

// defaultDataDir is the per-user state directory, matching what the
// packaging READMEs document: %LOCALAPPDATA%\Crispy Companion on Windows,
// ~/Library/Application Support/Crispy Companion on macOS, and the XDG
// data dir on Linux and everything else.
func defaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, "Crispy Companion")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "Crispy Companion")
		}
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "crispy-companion")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".local", "share", "crispy-companion")
}

// defaultDeviceName is the human label shown on the approval screen.
func defaultDeviceName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	name := "Crispy Companion on " + host
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

// revalidateLoop periodically checks the stored cp_pat_ against the
// server. A dead PAT (401) wipes local tokens and posts a warning;
// anything else (including offline) leaves them untouched.
func revalidateLoop(g *grant.Client, session *pairing.SessionFile, tokens *pairing.Store, notifier notify.Notifier, logger *log.Logger) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ds, ok := session.Get()
		if !ok || ds.PlaintextPAT == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := g.Revalidate(ctx, ds.PlaintextPAT)
		cancel()
		if err == nil {
			continue
		}
		if err != grant.ErrPATDead {
			logger.Printf("revalidate: server unreachable, keeping tokens")
			continue
		}
		logger.Printf("revalidate: device token dead, wiping local tokens")
		_ = tokens.RevokeAll()
		_ = session.Clear()
		_ = notifier.Post(notify.Notification{
			Title: "Crispy Companion unpaired",
			Body:  "This device was revoked. Pair again from Crispy settings.",
		})
	}
}

// stringHolder is a tiny mutex-guarded string (the userspace Go install
// in this container ships a corrupt sync/atomic without String).
type stringHolder struct {
	mu sync.RWMutex
	s  string
}

func (h *stringHolder) Store(s string) { h.mu.Lock(); h.s = s; h.mu.Unlock() }
func (h *stringHolder) Load() string   { h.mu.RLock(); defer h.mu.RUnlock(); return h.s }
