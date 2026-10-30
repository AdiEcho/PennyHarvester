// Package netproxy owns the outbound HTTP egress of the backend. The proxy
// configuration lives in the database and is editable from the web UI, so the
// HTTP clients have to be rebuilt whenever it changes.
package netproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"
)

type Mode string

const (
	// ModeEnvironment follows the process HTTP_PROXY / HTTPS_PROXY / NO_PROXY
	// variables. It is the default so an upgrade does not silently change how
	// an existing deployment reaches Polymarket.
	ModeEnvironment Mode = "environment"
	// ModeCustom routes every outbound request through URL, honouring NoProxy.
	ModeCustom Mode = "custom"
	// ModeOff connects directly and ignores the process environment.
	ModeOff Mode = "off"
)

// RedactedSecret replaces the proxy password in API responses.
const RedactedSecret = "******"

type Config struct {
	Mode    Mode   `json:"mode"`
	URL     string `json:"url"`
	NoProxy string `json:"no_proxy"`
}

func DefaultConfig() Config {
	return Config{Mode: ModeEnvironment}
}

func (c Config) Normalize() Config {
	c.Mode = Mode(strings.ToLower(strings.TrimSpace(string(c.Mode))))
	if c.Mode == "" {
		c.Mode = ModeEnvironment
	}
	c.URL = strings.TrimSpace(c.URL)
	c.NoProxy = strings.TrimSpace(c.NoProxy)
	return c
}

func (c Config) Validate() error {
	cfg := c.Normalize()
	switch cfg.Mode {
	case ModeEnvironment, ModeOff:
		return nil
	case ModeCustom:
		if cfg.URL == "" {
			return fmt.Errorf("自定义代理模式需要填写代理地址")
		}
		parsed, err := url.Parse(cfg.URL)
		if err != nil {
			return fmt.Errorf("代理地址无效: %w", err)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("代理协议仅支持 http、https 或 socks5")
		}
		if parsed.Host == "" {
			return fmt.Errorf("代理地址缺少主机与端口")
		}
		if parsed.User != nil && parsed.User.Username() == "" && parsed.Host == "" {
			return fmt.Errorf("代理地址格式无效")
		}
		for _, entry := range strings.Split(cfg.NoProxy, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			if strings.Contains(entry, "://") {
				return fmt.Errorf("NO_PROXY 条目不能包含协议: %s", entry)
			}
		}
		return nil
	default:
		return fmt.Errorf("代理模式仅支持 environment、custom 或 off")
	}
}

// Redacted returns a copy safe to send to the browser: the proxy password is
// replaced but the rest of the URL is preserved so the form stays readable.
func (c Config) Redacted() Config {
	cfg := c.Normalize()
	cfg.URL = redactURL(cfg.URL)
	return cfg
}

// MergeSecret restores the stored password when the client echoes back the
// redacted URL it received, so editing other fields does not wipe the secret.
func (c Config) MergeSecret(previous Config) Config {
	cfg := c.Normalize()
	previous = previous.Normalize()
	if cfg.Mode == ModeCustom && strings.Contains(cfg.URL, RedactedSecret) && redactURL(previous.URL) == cfg.URL {
		cfg.URL = previous.URL
	}
	return cfg
}

func redactURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User == nil {
		return strings.TrimSpace(raw)
	}
	if _, ok := parsed.User.Password(); !ok {
		return strings.TrimSpace(raw)
	}
	parsed.User = url.UserPassword(parsed.User.Username(), RedactedSecret)
	return strings.Replace(parsed.String(), url.PathEscape(RedactedSecret), RedactedSecret, 1)
}

// Manager holds the live proxy configuration and the HTTP clients built from
// it. Clients are pooled per timeout and invalidated as a group so a change
// made in the UI applies to the next request without a restart.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	proxyFn  func(*http.Request) (*url.URL, error)
	proxyURL *url.URL
	clients  map[time.Duration]*http.Client
}

func New(cfg Config) (*Manager, error) {
	manager := &Manager{clients: map[time.Duration]*http.Client{}}
	if err := manager.Apply(cfg); err != nil {
		return nil, err
	}
	return manager, nil
}

// NewMust builds a manager and falls back to the environment mode when the
// stored configuration is unusable, so a bad row cannot brick startup.
func NewMust(cfg Config) *Manager {
	manager, err := New(cfg)
	if err == nil {
		return manager
	}
	fallback, _ := New(DefaultConfig())
	return fallback
}

func (m *Manager) Apply(cfg Config) error {
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	proxyFn, proxyURL, err := proxyFuncFor(cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	stale := m.clients
	m.cfg = cfg
	m.proxyFn = proxyFn
	m.proxyURL = proxyURL
	m.clients = map[time.Duration]*http.Client{}
	m.mu.Unlock()
	for _, client := range stale {
		client.CloseIdleConnections()
	}
	return nil
}

func (m *Manager) Config() Config {
	if m == nil {
		return DefaultConfig()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// ProxyURL reports the explicit proxy for clients that build their own
// transport (the CLOB SDK). It is nil for environment and off modes: the
// environment mode is meant to be resolved by http.ProxyFromEnvironment, and
// off mode needs a client with no proxy function at all.
func (m *Manager) ProxyURL() *url.URL {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proxyURL
}

// HTTPClient returns a shared client for the given timeout. The same client is
// reused until the configuration changes, which keeps connection pooling.
func (m *Manager) HTTPClient(timeout time.Duration) *http.Client {
	if m == nil {
		return &http.Client{Timeout: timeout}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if client, ok := m.clients[timeout]; ok {
		return client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = m.proxyFn
	client := &http.Client{Timeout: timeout, Transport: transport}
	m.clients[timeout] = client
	return client
}

func proxyFuncFor(cfg Config) (func(*http.Request) (*url.URL, error), *url.URL, error) {
	switch cfg.Mode {
	case ModeOff:
		return func(*http.Request) (*url.URL, error) { return nil, nil }, nil, nil
	case ModeEnvironment:
		return http.ProxyFromEnvironment, nil, nil
	case ModeCustom:
		parsed, err := url.Parse(cfg.URL)
		if err != nil {
			return nil, nil, fmt.Errorf("解析代理地址: %w", err)
		}
		resolver := httpproxy.Config{
			HTTPProxy:  cfg.URL,
			HTTPSProxy: cfg.URL,
			NoProxy:    cfg.NoProxy,
		}
		resolve := resolver.ProxyFunc()
		return func(req *http.Request) (*url.URL, error) {
			return resolve(req.URL)
		}, parsed, nil
	default:
		return nil, nil, fmt.Errorf("代理模式仅支持 environment、custom 或 off")
	}
}
