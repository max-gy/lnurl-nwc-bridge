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
		"coffee": {"description": "Coffee", "eur_cents": 350, "wallet": "shop"}
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
	if !ok || product.Description != "Coffee" || product.EURCents != 350 || product.Wallet != "shop" {
		t.Fatalf("unexpected products: %#v", cfg.Products)
	}
}

func TestLoadConfigRejectsInvalidProductsFile(t *testing.T) {
	for _, tt := range []struct {
		name     string
		products string
		inline   string
	}{
		{name: "unknown product field", products: `{"coffee":{"description":"Coffee","eur_cents":350,"wallet":"shop","unexpected":true}}`},
		{name: "invalid product", products: `{"coffee":{"description":"Coffee","eur_cents":0,"wallet":"shop"}}`},
		{name: "unknown wallet", products: `{"coffee":{"description":"Coffee","eur_cents":350,"wallet":"other"}}`},
		{name: "multiple JSON values", products: `{} {}`},
		{name: "inline products also configured", products: `{}`, inline: `,"products":{"coffee":{"description":"Coffee","eur_cents":350,"wallet":"shop"}}`},
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

func TestLoadConfigInlineProductsRemainSupported(t *testing.T) {
	configPath := writeTestConfig(t, "config.json", `{
		"public_url": "https://pay.example.com",
		"db_path": "/var/lib/lnurl-api/quotes.db",
		"wallets": {"shop": {"nwc_uri_file": "/run/secrets/nwc"}},
		"products": {"coffee": {"description": "Coffee", "eur_cents": 350, "wallet": "shop"}}
	}`)
	if _, err := LoadConfig(configPath); err != nil {
		t.Fatal(err)
	}
}
