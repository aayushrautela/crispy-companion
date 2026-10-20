// Package config loads crispy-companion configuration.
//
// Precedence: defaults < config file (TOML subset) < environment overrides.
//
// The TOML reader is intentionally a small subset parser (sections, string,
// int, bool, string arrays) so the M1 skeleton stays dependency-free.
// Unknown keys are ignored.
package config

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the full companion configuration. See PLAN.md §9.
type Config struct {
	ListenAddr string
	ListenPort int

	EngineAddr          string
	EnginePort          int
	EngineAutoStart     bool
	EngineBinaryPath    string
	EngineMaxCacheBytes int64

	// /v1 readiness policy (spec §1.1): time-based buffer gating.
	BufferMinBytes                 int64
	BufferStartSeconds             float64
	BufferBitrateWindowSeconds     float64
	BufferSustainableMarginPercent float64

	AllowedOrigins            []string
	RequireOnlineVerification bool
	PendingTTLSeconds         int
	MaxAttemptsPerMinute      int

	CrispyServerURL string

	SharedMode       string
	SharedListenAddr string

	LogLevel string
	DataDir  string
}

// buildServerURL and buildAllowedOrigins are deployment defaults injected at
// build time. CI generates builddefaults_gen.go from repository secrets (see
// .github/workflows/flatpak.yml); that file is gitignored, so local builds
// keep the fail-closed OSS defaults below. Config file values and
// environment variables always take precedence over these.
var buildServerURL = ""

// buildAllowedOrigins is a comma-separated origin list, e.g.
// "https://web.crispytv.tech".
var buildAllowedOrigins = ""

// Default returns the M1 default configuration (loopback-only).
func Default() Config {
	cfg := Config{
		ListenAddr: "127.0.0.1",
		ListenPort: 11470,

		EngineAddr:      "127.0.0.1",
		EnginePort:      8090,
		EngineAutoStart: true,
		// The pinned engine binary, resolved through PATH: the Flatpak
		// installs it at /app/bin/torrserver. Leaving this empty means the
		// supervisor never spawns an engine, so torrents cannot play — the
		// supervisor degrades gracefully (no error) when the binary is
		// genuinely absent.
		EngineBinaryPath: "torrserver",

		BufferMinBytes:                 4 << 20,
		BufferStartSeconds:             10,
		BufferBitrateWindowSeconds:     10,
		BufferSustainableMarginPercent: 100,

		AllowedOrigins: []string{
			"https://crispy.app",
			"https://app.crispy.example",
		},
		RequireOnlineVerification: true,
		PendingTTLSeconds:         90,
		MaxAttemptsPerMinute:      5,

		CrispyServerURL: normalizeServerURL(buildServerURL),
		SharedMode:      "off",

		LogLevel: "info",
	}
	for _, o := range strings.Split(buildAllowedOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.AllowedOrigins = append(cfg.AllowedOrigins, o)
		}
	}
	return cfg
}

// normalizeServerURL tidies a Crispy server URL and keeps the device grant
// off plain HTTP.
//
// The trailing slash is trimmed (the grant client appends "/v1/…"), and a
// non-loopback http:// host is upgraded to https://. The grant poll carries
// the device code and its reply carries the cp_pat_, so neither may travel
// in cleartext. Loopback keeps http so local test servers still work.
func normalizeServerURL(raw string) string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" || !strings.HasPrefix(u, "http://") {
		return u
	}
	host := strings.TrimPrefix(u, "http://")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return u
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return u
	}
	return "https://" + host
}

// DataFile resolves a file inside the data dir.
func (c Config) DataFile(name string) string {
	return filepath.Join(c.DataDir, name)
}

// Load resolves the data dir, optionally reads a TOML file, then applies env overrides.
func Load(dataDir, configPath string) (Config, error) {
	cfg := Default()
	cfg.DataDir = dataDir

	if configPath != "" {
		if err := loadFile(configPath, &cfg); err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("load config %s: %w", configPath, err)
			}
		}
	} else {
		def := filepath.Join(dataDir, "config.toml")
		if err := loadFile(def, &cfg); err != nil && !os.IsNotExist(err) {
			return cfg, fmt.Errorf("load config %s: %w", def, err)
		}
	}

	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate rejects unsafe effective configuration.
func (c Config) Validate() error {
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("invalid listen port %d", c.ListenPort)
	}
	if c.SharedMode != "off" && c.SharedMode != "raw" {
		return fmt.Errorf("invalid shared.mode %q (want off|raw)", c.SharedMode)
	}
	if c.SharedMode == "raw" {
		// Raw LAN is plaintext HTTP: demand an explicit, non-loopback,
		// non-unspecified bind address. Never 0.0.0.0 by default, never
		// a second loopback listener masquerading as "shared".
		ip := net.ParseIP(c.SharedListenAddr)
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
			return fmt.Errorf("shared.mode=raw requires an explicit LAN shared.listenAddr (got %q)", c.SharedListenAddr)
		}
	}
	for _, o := range c.AllowedOrigins {
		if o == "*" || strings.Contains(o, "*") {
			return fmt.Errorf("allowedOrigins must be exact origins, got %q", o)
		}
	}

	if c.BufferMinBytes < 0 || c.BufferStartSeconds < 0 || c.BufferBitrateWindowSeconds < 0 || c.BufferSustainableMarginPercent < 0 {
		return fmt.Errorf("invalid buffer policy (negative values)")
	}
	return nil
}

// Addr returns host:port for the shim listener.
func (c Config) Addr() string {
	return c.ListenAddr + ":" + strconv.Itoa(c.ListenPort)
}

// EngineEndpoint returns host:port for the TorrServer engine (loopback only).
func (c Config) EngineEndpoint() string {
	return c.EngineAddr + ":" + strconv.Itoa(c.EnginePort)
}

func applyEnv(c *Config) {
	if v := os.Getenv("CRISPY_COMPANION_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("CRISPY_COMPANION_LISTEN_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.ListenPort = p
		}
	}
	if v := os.Getenv("CRISPY_COMPANION_SERVER_URL"); v != "" {
		// Tolerate a trailing slash: the grant client appends "/v1/…".
		c.CrispyServerURL = normalizeServerURL(v)
	}
	if v := os.Getenv("CRISPY_COMPANION_SHARED_MODE"); v != "" {
		c.SharedMode = v
	}
	if v := os.Getenv("CRISPY_COMPANION_SHARED_LISTEN_ADDR"); v != "" {
		c.SharedListenAddr = v
	}
	if v := os.Getenv("CRISPY_COMPANION_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("CRISPY_COMPANION_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("CRISPY_COMPANION_ALLOWED_ORIGINS"); v != "" {
		c.AllowedOrigins = strings.Split(v, ",")
		for i := range c.AllowedOrigins {
			c.AllowedOrigins[i] = strings.TrimSpace(c.AllowedOrigins[i])
		}
	}
}

// loadFile parses a small TOML subset into cfg.
func loadFile(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	section := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"`))
		v = strings.Trim(v, `'`)
		set(section, k, v, cfg)
	}
	return sc.Err()
}

func set(section, key, val string, cfg *Config) {
	switch section + "." + key {
	case "listen.addr":
		cfg.ListenAddr = val
	case "listen.port":
		if p, err := strconv.Atoi(val); err == nil {
			cfg.ListenPort = p
		}
	case "engine.addr":
		cfg.EngineAddr = val
	case "engine.port":
		if p, err := strconv.Atoi(val); err == nil {
			cfg.EnginePort = p
		}
	case "engine.autoStart":
		cfg.EngineAutoStart = parseBool(val)
	case "engine.binaryPath":
		cfg.EngineBinaryPath = val
	case "engine.maxCacheBytes":
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			cfg.EngineMaxCacheBytes = n
		}
	case "buffer.minBytes":
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			cfg.BufferMinBytes = n
		}
	case "buffer.startSeconds":
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			cfg.BufferStartSeconds = f
		}
	case "buffer.bitrateWindowSeconds":
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			cfg.BufferBitrateWindowSeconds = f
		}
	case "buffer.sustainableMarginPercent":
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			cfg.BufferSustainableMarginPercent = f
		}
	case "auth.requireOnlineVerification":
		cfg.RequireOnlineVerification = parseBool(val)
	case "pairing.pendingTTLSeconds":
		if n, err := strconv.Atoi(val); err == nil {
			cfg.PendingTTLSeconds = n
		}
	case "pairing.maxAttemptsPerMinute":
		if n, err := strconv.Atoi(val); err == nil {
			cfg.MaxAttemptsPerMinute = n
		}
	case "crispyServer.url":
		cfg.CrispyServerURL = normalizeServerURL(val)
	case "shared.mode":
		cfg.SharedMode = val
	case "shared.listenAddr":
		cfg.SharedListenAddr = val
	case "logging.level":
		cfg.LogLevel = val
	case "auth.allowedOrigins", "auth.tokenFile":
		// allowedOrigins handled below; tokenFile is fixed name in M1.
		if key == "allowedOrigins" {
			cfg.AllowedOrigins = parseStringArray(val)
		}
	}
}

func parseBool(s string) bool {
	switch strings.ToLower(s) {
	case "true", "yes", "1", "on":
		return true
	default:
		return false
	}
}

func parseStringArray(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"`))
		p = strings.Trim(p, `'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
