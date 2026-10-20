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
	"strconv"
	"sync"
	"syscall"
	"time"

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
	requestAutostart := fs.Bool("request-autostart", false, "ask the desktop (portal) for background + login autostart, then run")
	menuLaunch := fs.Bool("menu-launch", false, "started from the desktop start-menu entry: report the outcome as a notification")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	return runDaemon(*requestAutostart, *menuLaunch)
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

func runDaemon(requestAutostart, menuLaunch bool) error {
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
	if pc, err := portal.Dial(); err != nil {
		logger.Printf("portal unavailable, notifications go to log: %v", err)
	} else {
		defer pc.Close()
		notifier = notify.NewPortal(pc, func(id, action string) {
			if id != "crispy-pairing" || action != "revoke" {
				return
			}
			logger.Printf("revoke requested from notification")
			_ = tokens.RevokeAll()
			_ = session.Clear()
		})
		if requestAutostart {
			bg, auto, err := pc.RequestBackground("Keep torrent streaming available in the background", true, 30*time.Second)
			if err != nil {
				logger.Printf("autostart request failed: %v", err)
			} else {
				logger.Printf("autostart request: background=%v autostart=%v", bg, auto)
			}
		}
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
	// answer the click with something visible. Unconditional: the click
	// must never look like a no-op, and the flag may not survive the
	// desktop's Exec rewriting.
	if menuLaunch {
		logger.Printf("launched from the start-menu entry")
	}
	_ = notifier.Post(notify.Notification{
		Title: "Crispy Companion started",
		Body:  "Listening on 127.0.0.1:" + strconv.Itoa(port) + ". Open the Crispy web app to pair this device.",
	})

	// M5 shared listeners. Loopback always serves; these add a second
	// surface. The engine stays loopback-only in every mode.
	switch cfg.SharedMode {
	case "raw":
		if err := serveRaw(cfg, port, hosts, origins, dev, deps, sharedAddr, track, logger); err != nil {
			return err
		}
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

func defaultDataDir() string {
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
