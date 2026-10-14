// Command crispy-companion is the headless Crispy companion daemon.
//
// M1: loopback listener with perimeter middleware, engine supervisor,
// file-backed config + token store. No GUI; logs go to stderr (Flatpak
// captures them via journal).
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/config"
	"github.com/crispy-streaming/crispy-companion/internal/engine"
	"github.com/crispy-streaming/crispy-companion/internal/grant"
	"github.com/crispy-streaming/crispy-companion/internal/httpx"
	"github.com/crispy-streaming/crispy-companion/internal/notify"
	"github.com/crispy-streaming/crispy-companion/internal/pairing"
	"github.com/crispy-streaming/crispy-companion/internal/server"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("crispy-companion: %v", err)
	}
}

func run() error {
	logger := log.New(os.Stderr, "crispy-companion: ", log.LstdFlags)

	dataDir := defaultDataDir()
	if v := os.Getenv("CRISPY_COMPANION_DATA_DIR"); v != "" {
		dataDir = v
	}
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
	defer eng.Stop()

	mode := "loopback"
	if cfg.SharedMode != "off" {
		mode = cfg.SharedMode
	}
	notifier := notify.LogNotifier{Log: logger}
	handler := server.New(server.Deps{
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
	})

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
		return err
	}
	// Refuse to run if we somehow bound off-loopback.
	if addr, ok := ln.Addr().(*net.TCPAddr); ok && !addr.IP.IsLoopback() {
		_ = ln.Close()
		return errors.New("refusing to serve on non-loopback address in M1")
	}
	logger.Printf("listening on http://%s (mode=%s)", ln.Addr(), mode)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("server error: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
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
