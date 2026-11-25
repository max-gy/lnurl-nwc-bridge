package pay

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeTestConfig(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigProductsFile(t *testing.T) {
	productsPath := writeTestConfig(t, "products.json", `{
		"coffee": {"description": "Coffee", "currency": "EUR", "price": 350, "wallet": "shop"}
	}`)
	configPath := writeTestConfig(t, "config.json", `{
		"public_url": "https://pay.example.com",
		"db_path": "/var/lib/lnurl-api/quotes.db",
		"products_file": `+strconv.Quote(productsPath)+`,
		"wallets": {"shop": {"nwc_uri_file": "/run/secrets/nwc"}}
	}`)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	product, ok := cfg.Products["coffee"]
	if !ok || product.Description != "Coffee" || product.Currency != "EUR" || product.Price != 350 || product.Wallet != "shop" {
		t.Fatalf("unexpected products: %#v", cfg.Products)
	}
}

func TestLoadConfigRejectsInvalidProductsFile(t *testing.T) {
	for _, tt := range []struct {
		name     string
		products string
		inline   string
	}{
		{name: "unknown product field", products: `{"coffee":{"description":"Coffee","currency":"EUR","price":350,"wallet":"shop","unexpected":true}}`},
		{name: "invalid product", products: `{"coffee":{"description":"Coffee","currency":"EUR","price":0,"wallet":"shop"}}`},
		{name: "unknown wallet", products: `{"coffee":{"description":"Coffee","currency":"EUR","price":350,"wallet":"other"}}`},
		{name: "multiple JSON values", products: `{} {}`},
		{name: "inline products also configured", products: `{}`, inline: `,"products":{"coffee":{"description":"Coffee","currency":"EUR","price":350,"wallet":"shop"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			productsPath := writeTestConfig(t, "products.json", tt.products)
			configPath := writeTestConfig(t, "config.json", `{
				"public_url": "https://pay.example.com",
				"db_path": "/var/lib/lnurl-api/quotes.db",
				"products_file": `+strconv.Quote(productsPath)+`,
				"wallets": {"shop": {"nwc_uri_file": "/run/secrets/nwc"}}`+tt.inline+`
			}`)
			if _, err := LoadConfig(configPath); err == nil {
				t.Fatal("expected configuration to be rejected")
			}
		})
	}
}

func TestValidateVariableAmountProduct(t *testing.T) {
	base := Config{
		Listen: ":8080", PublicURL: "https://pay.example.com", DBPath: "/var/lib/lnurl-api/quotes.db",
		Network: "mainnet", QuoteTTLSeconds: 120, InvoiceTTLSeconds: 300,
		RequestTimeoutSeconds: 15, RateMaxAgeSeconds: 120, RequestsPerSecond: 5,
		Burst: 20, MaxQuotes: 10000,
		Wallets: map[string]WalletConfig{"shop": {URIFile: "/run/secrets/nwc"}},
	}
	for _, tt := range []struct {
		name    string
		product Product
		valid   bool
	}{
		{name: "EUR fixed price", product: Product{Description: "Coffee", Currency: "EUR", Price: 100, Wallet: "shop"}, valid: true},
		{name: "USD fixed price", product: Product{Description: "Coffee", Currency: "USD", Price: 100, Wallet: "shop"}, valid: true},
		{name: "JPY fixed price", product: Product{Description: "Coffee", Currency: "JPY", Price: 100, Wallet: "shop"}, valid: true},
		{name: "lowercase currency", product: Product{Description: "Coffee", Currency: "eur", Price: 100, Wallet: "shop"}},
		{name: "unknown currency", product: Product{Description: "Coffee", Currency: "INVALID", Price: 100, Wallet: "shop"}},
		{name: "missing currency", product: Product{Description: "Coffee", Price: 100, Wallet: "shop"}},
		{name: "donation", product: Product{Description: "Donation", MinSats: 21, MaxSats: 100000, Wallet: "shop"}, valid: true},
		{name: "fixed donation", product: Product{Description: "Donation", MinSats: 21, MaxSats: 21, Wallet: "shop"}, valid: true},
		{name: "missing maximum", product: Product{Description: "Donation", MinSats: 21, Wallet: "shop"}},
		{name: "reversed limits", product: Product{Description: "Donation", MinSats: 100, MaxSats: 21, Wallet: "shop"}},
		{name: "fixed and variable", product: Product{Description: "Donation", Currency: "EUR", Price: 100, MinSats: 21, MaxSats: 100, Wallet: "shop"}},
		{name: "no amount", product: Product{Description: "Donation", Wallet: "shop"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Products = map[string]Product{"donation": tt.product}
			if err := cfg.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate() error = %v; valid = %v", err, tt.valid)
			}
		})
	}
}

func TestLoadConfigInlineProductsRemainSupported(t *testing.T) {
	configPath := writeTestConfig(t, "config.json", `{
		"public_url": "https://pay.example.com",
		"db_path": "/var/lib/lnurl-api/quotes.db",
		"wallets": {"shop": {"nwc_uri_file": "/run/secrets/nwc"}},
		"products": {"coffee": {"description": "Coffee", "currency": "EUR", "price": 350, "wallet": "shop"}}
	}`)
	if _, err := LoadConfig(configPath); err != nil {
		t.Fatal(err)
	}
}
