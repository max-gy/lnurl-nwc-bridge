package pay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"time"
)

type WalletConfig struct {
	URIFile          string `json:"nwc_uri_file"`
	AllowLegacyNIP04 bool   `json:"allow_legacy_nip04"`
}

type Product struct {
	Description string `json:"description"`
	EURCents    int64  `json:"eur_cents"`
	Wallet      string `json:"wallet"`
}

type Config struct {
	Listen                string                  `json:"listen"`
	PublicURL             string                  `json:"public_url"`
	DBPath                string                  `json:"db_path"`
	Network               string                  `json:"network"`
	QuoteTTLSeconds       int                     `json:"quote_ttl_seconds"`
	InvoiceTTLSeconds     int                     `json:"invoice_ttl_seconds"`
	RequestTimeoutSeconds int                     `json:"request_timeout_seconds"`
	RateMaxAgeSeconds     int                     `json:"rate_max_age_seconds"`
	RequestsPerSecond     float64                 `json:"requests_per_second"`
	Burst                 int                     `json:"burst"`
	MaxQuotes             int                     `json:"max_quotes"`
	Wallets               map[string]WalletConfig `json:"wallets"`
	Products              map[string]Product      `json:"products"`
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		Listen: ":8080", Network: "mainnet",
		QuoteTTLSeconds: 120, InvoiceTTLSeconds: 300, RequestTimeoutSeconds: 15,
		RateMaxAgeSeconds: 120, RequestsPerSecond: 5, Burst: 20, MaxQuotes: 10000,
	}
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("configuration must contain exactly one JSON object")
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("public_url must be an HTTPS origin without credentials, path, query or fragment")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return errors.New("listen must be a host:port address")
	}
	if c.DBPath == "" {
		return errors.New("db_path is required")
	}
	if c.Network != "mainnet" && c.Network != "testnet" && c.Network != "regtest" && c.Network != "signet" {
		return errors.New("network must be mainnet, testnet, signet or regtest")
	}
	if c.QuoteTTLSeconds < 10 || c.QuoteTTLSeconds > 600 ||
		c.InvoiceTTLSeconds < 30 || c.InvoiceTTLSeconds > 3600 ||
		c.RequestTimeoutSeconds < 1 || c.RequestTimeoutSeconds > 60 ||
		c.RequestTimeoutSeconds >= c.QuoteTTLSeconds ||
		c.RateMaxAgeSeconds < 30 || c.RateMaxAgeSeconds > 600 {
		return errors.New("invalid TTL, rate age or request timeout")
	}
	if c.RequestsPerSecond <= 0 || c.RequestsPerSecond > 1000 || c.Burst < 1 ||
		c.Burst > 10000 || c.MaxQuotes < 1 || c.MaxQuotes > 1000000 {
		return errors.New("invalid request or storage limits")
	}
	if len(c.Products) == 0 || len(c.Wallets) == 0 {
		return errors.New("at least one wallet and product are required")
	}
	for name, w := range c.Wallets {
		if !identifier.MatchString(name) || w.URIFile == "" {
			return errors.New("wallet names must be identifiers and nwc_uri_file is required")
		}
	}
	for id, p := range c.Products {
		if !identifier.MatchString(id) || p.Description == "" || len(p.Description) > 1024 ||
			p.EURCents <= 0 || p.EURCents > 100000000 {
			return errors.New("invalid product identifier, description or EUR cents")
		}
		if _, ok := c.Wallets[p.Wallet]; !ok {
			return fmt.Errorf("product %s references an unknown wallet", id)
		}
	}
	return nil
}

func (c Config) Timeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}
