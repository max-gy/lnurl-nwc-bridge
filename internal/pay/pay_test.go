package pay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestQuoteMSats(t *testing.T) {
	for _, tt := range []struct {
		name     string
		currency string
		price    int64
		rate     string
		want     int64
	}{
		{name: "EUR minor units", currency: "EUR", price: 350, rate: "70000", want: 5000000},
		{name: "USD minor units", currency: "USD", price: 100, rate: "60000", want: 1667000},
		{name: "JPY major units", currency: "JPY", price: 100, rate: "10000000", want: 1000000},
		{name: "KWD minor units", currency: "KWD", price: 1000, rate: "30000", want: 3334000},
		{name: "decimal rate", currency: "EUR", price: 350, rate: "70000.01", want: 5000000},
		{name: "invalid rate", currency: "EUR", price: 100, rate: "0"},
		{name: "invalid price", currency: "EUR", price: -1, rate: "60000"},
		{name: "invalid currency", currency: "INVALID", price: 100, rate: "60000"},
		{name: "overflow", currency: "EUR", price: 100000000, rate: "0.000000001"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rate, _ := new(big.Rat).SetString(tt.rate)
			got, err := QuoteMSats(tt.currency, tt.price, rate)
			if tt.want == 0 {
				if err == nil {
					t.Fatal("expected invalid price/rate to fail")
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("got %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

func TestRatesFreshnessAndCache(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		ok   bool
	}{
		{"fresh", `{"bitcoin":{"eur":70000.25,"last_updated_at":%d}}`, true},
		{"stale", `{"bitcoin":{"eur":70000.25,"last_updated_at":1}}`, false},
		{"missing time", `{"bitcoin":{"eur":70000.25}}`, false},
		{"invalid price", `{"bitcoin":{"eur":-1,"last_updated_at":%d}}`, false},
		{"malformed", `{"bitcoin":`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body := strings.ReplaceAll(tt.body, "%d", big.NewInt(time.Now().Unix()).String())
				io.WriteString(w, body)
			}))
			defer server.Close()
			rates := NewRates(2 * time.Minute)
			rates.url, rates.client = server.URL, server.Client()
			_, err := rates.FiatPerBTC(context.Background(), "EUR")
			if (err == nil) != tt.ok {
				t.Fatalf("unexpected rate error: %v", err)
			}
			if tt.ok {
				if _, err := rates.FiatPerBTC(context.Background(), "EUR"); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 1 {
					t.Fatal("fresh rate was not cached")
				}
			}
		})
	}
	t.Run("separate currency caches", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			code := r.URL.Query().Get("vs_currencies")
			if code != "eur" && code != "usd" {
				t.Errorf("unexpected currency: %q", code)
			}
			fmt.Fprintf(w, `{"bitcoin":{%q:70000,"last_updated_at":%d}}`, code, time.Now().Unix())
		}))
		defer server.Close()
		rates := NewRates(time.Minute)
		rates.url, rates.client = server.URL, server.Client()
		for _, code := range []string{"EUR", "USD", "EUR", "USD"} {
			if _, err := rates.FiatPerBTC(context.Background(), code); err != nil {
				t.Fatal(err)
			}
		}
		if calls.Load() != 2 {
			t.Fatalf("expected one request per currency, got %d", calls.Load())
		}
	})
	t.Run("no stale fallback", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer server.Close()
		rates := NewRates(time.Minute)
		rates.url, rates.client = server.URL, server.Client()
		rates.rates["eur"] = cachedRate{
			value: big.NewRat(70000, 1), updated: time.Now().Add(-time.Hour), fetched: time.Now().Add(-time.Hour),
		}
		if _, err := rates.FiatPerBTC(context.Background(), "EUR"); err == nil {
			t.Fatal("provider failure must not return an old rate")
		}
	})
}

func signedInvoice(amount int64, hash [32]byte, timestamp time.Time, expiry time.Duration) (WalletInvoice, error) {
	paymentHash := sha256.Sum256([]byte("test-payment-" + hex.EncodeToString(hash[:])))
	invoice, err := zpay32.NewInvoice(&chaincfg.MainNetParams, paymentHash, timestamp,
		zpay32.Amount(lnwire.MilliSatoshi(amount)), zpay32.DescriptionHash(hash), zpay32.Expiry(expiry))
	if err != nil {
		return WalletInvoice{}, err
	}
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	encoded, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(message []byte) ([]byte, error) {
			digest := sha256.Sum256(message)
			return ecdsa.SignCompact(key, digest[:], true), nil
		},
	})
	return WalletInvoice{Invoice: encoded, PaymentHash: hex.EncodeToString(paymentHash[:]), Amount: amount}, err
}

type fixedRates struct {
	value *big.Rat
	err   error
}

func (r fixedRates) FiatPerBTC(context.Context, string) (*big.Rat, error) {
	return r.value, r.err
}

type fakeWallet struct {
	calls   atomic.Int32
	fail    bool
	offline bool
	tamper  bool
	status  WalletStatus
}

func (f *fakeWallet) Ready() bool { return !f.offline }
func (f *fakeWallet) MakeInvoice(ctx context.Context, amount int64, hash string, expiry int) (WalletInvoice, error) {
	f.calls.Add(1)
	if f.fail {
		return WalletInvoice{}, errors.New("simulated uncertain delivery")
	}
	raw, err := hex.DecodeString(hash)
	if err != nil {
		return WalletInvoice{}, err
	}
	var digest [32]byte
	copy(digest[:], raw)
	if f.tamper {
		amount++
	}
	return signedInvoice(amount, digest, time.Now(), time.Duration(expiry)*time.Second)
}

func (f *fakeWallet) LookupInvoice(ctx context.Context, hash string) (WalletStatus, error) {
	result := f.status
	result.PaymentHash = hash
	return result, nil
}

func newTestAPI(t *testing.T, wallet *fakeWallet) (*API, http.Handler) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "quotes.db"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := Config{
		PublicURL: "https://pay.example.com", Network: "mainnet",
		QuoteTTLSeconds: 120, InvoiceTTLSeconds: 300, RequestTimeoutSeconds: 2,
		RequestsPerSecond: 10000, Burst: 10000,
		Products: map[string]Product{
			"coffee": {Description: "Coffee <large>", Currency: "EUR", Price: 350, Wallet: "shop"},
		},
	}
	a := NewAPI(cfg, store, fixedRates{value: big.NewRat(70000, 1)},
		map[string]Wallet{"shop": wallet}, testLogger())
	return a, a.Handler()
}

func getJSON(t *testing.T, handler http.Handler, path string) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected HTTP status %d: %s", rec.Code, rec.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("payment responses must not be cached")
	}
	return result
}

func stringField(t *testing.T, data map[string]json.RawMessage, field string) string {
	t.Helper()
	var result string
	if err := json.Unmarshal(data[field], &result); err != nil {
		t.Fatalf("invalid %s: %v; response=%v", field, err, data)
	}
	return result
}

func newQuote(t *testing.T, handler http.Handler) (string, string) {
	t.Helper()
	data := getJSON(t, handler, "/lnurl/coffee")
	if stringField(t, data, "tag") != "payRequest" ||
		string(data["minSendable"]) != "5000000" || string(data["maxSendable"]) != "5000000" {
		t.Fatalf("incorrect fixed-price LNURL shape: %v", data)
	}
	callback := strings.TrimPrefix(stringField(t, data, "callback"), "https://pay.example.com")
	return callback, stringField(t, data, "metadata")
}

func TestVariableAmountDonation(t *testing.T) {
	wallet := &fakeWallet{}
	api, handler := newTestAPI(t, wallet)
	api.cfg.Products["donation"] = Product{
		Description: "Donation", MinSats: 21, MaxSats: 100000, Wallet: "shop",
	}
	data := getJSON(t, handler, "/lnurl/donation")
	if string(data["minSendable"]) != "21000" || string(data["maxSendable"]) != "100000000" {
		t.Fatalf("incorrect donation limits: %v", data)
	}
	callback := strings.TrimPrefix(stringField(t, data, "callback"), "https://pay.example.com")
	first := getJSON(t, handler, callback+"?amount=12345000")
	invoice := stringField(t, first, "pr")
	decoded, err := zpay32.Decode(invoice, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.MilliSat == nil || *decoded.MilliSat != 12345000 {
		t.Fatalf("invoice does not contain selected donation amount: %v", decoded.MilliSat)
	}
	if wallet.calls.Load() != 1 {
		t.Fatalf("wallet calls = %d; want 1", wallet.calls.Load())
	}
	second := getJSON(t, handler, callback+"?amount=12345000")
	if stringField(t, second, "pr") != invoice || wallet.calls.Load() != 1 {
		t.Fatal("retry must return the original donation invoice")
	}
	getJSON(t, handler, callback+"?amount=12346000")
	if wallet.calls.Load() != 1 {
		t.Fatal("retry with another amount reached the wallet")
	}
}

func TestVariableAmountDonationRejectsOutOfRangeAmount(t *testing.T) {
	wallet := &fakeWallet{}
	api, handler := newTestAPI(t, wallet)
	api.cfg.Products["donation"] = Product{
		Description: "Donation", MinSats: 21, MaxSats: 1000, Wallet: "shop",
	}
	data := getJSON(t, handler, "/lnurl/donation")
	callback := strings.TrimPrefix(stringField(t, data, "callback"), "https://pay.example.com")
	for _, amount := range []string{"20999", "1000001"} {
		getJSON(t, handler, callback+"?amount="+amount)
	}
	if wallet.calls.Load() != 0 {
		t.Fatal("out-of-range donation reached the wallet")
	}
	getJSON(t, handler, callback+"?amount=21000")
	if wallet.calls.Load() != 1 {
		t.Fatal("rejected amount must not consume the donation quote")
	}
}

func TestLNURLQuoteInvoiceAndIdempotency(t *testing.T) {
	wallet := &fakeWallet{}
	api, handler := newTestAPI(t, wallet)
	callback, metadata := newQuote(t, handler)
	// A later exchange-rate change must not alter an already advertised quote.
	api.rates = fixedRates{value: big.NewRat(50000, 1)}
	first := getJSON(t, handler, callback+"?amount=5000000")
	invoice := stringField(t, first, "pr")
	if string(first["routes"]) != "[]" {
		t.Fatalf("routes must be an empty array: %s", first["routes"])
	}
	decoded, err := zpay32.Decode(invoice, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256([]byte(metadata))
	if decoded.MilliSat == nil || int64(*decoded.MilliSat) != 5000000 ||
		decoded.DescriptionHash == nil || *decoded.DescriptionHash != expectedHash {
		t.Fatal("invoice not bound to exact advertised metadata and amount")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, callback+"?amount=5000000", nil))
			var data struct {
				PR string `json:"pr"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil || data.PR != invoice {
				t.Errorf("duplicate callback did not reuse invoice: %s", rec.Body.String())
			}
		})
	}
	wg.Wait()
	if wallet.calls.Load() != 1 {
		t.Fatalf("created %d invoices for one quote", wallet.calls.Load())
	}
}

func TestCallbackRejectionsAndUncertainOutcome(t *testing.T) {
	for _, query := range []string{
		"", "?amount=4999999", "?amount=5000000&amount=5000000",
		"?amount=5000000.0", "?amount=-1", "?amount=%2b5000000",
		"?amount=5000000&broken=%zz", "?amount=9999999999999999999",
	} {
		t.Run(query, func(t *testing.T) {
			wallet := &fakeWallet{}
			_, handler := newTestAPI(t, wallet)
			callback, _ := newQuote(t, handler)
			data := getJSON(t, handler, callback+query)
			if stringField(t, data, "status") != "ERROR" || wallet.calls.Load() != 0 {
				t.Fatal("invalid amount reached the wallet")
			}
		})
	}
	for _, tamper := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "invalid invoice"}[tamper], func(t *testing.T) {
			wallet := &fakeWallet{fail: !tamper, tamper: tamper}
			_, handler := newTestAPI(t, wallet)
			callback, _ := newQuote(t, handler)
			for range 2 {
				data := getJSON(t, handler, callback+"?amount=5000000")
				if stringField(t, data, "status") != "ERROR" {
					t.Fatal("failed invoice request returned success")
				}
			}
			if wallet.calls.Load() != 1 {
				t.Fatal("uncertain request was automatically repeated")
			}
		})
	}
	t.Run("expired quote", func(t *testing.T) {
		wallet := &fakeWallet{}
		api, handler := newTestAPI(t, wallet)
		q := Quote{ID: strings.Repeat("a", 48), Wallet: "shop", Amount: 5000000, State: "new", Expires: time.Now().Unix() - 1}
		if err := api.store.Create(q); err != nil {
			t.Fatal(err)
		}
		data := getJSON(t, handler, "/quote/"+q.ID+"?amount=5000000")
		if stringField(t, data, "status") != "ERROR" || wallet.calls.Load() != 0 {
			t.Fatal("expired quote reached wallet")
		}
	})
}

func TestStoreRestartAndExclusiveClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.db")
	store, err := OpenStore(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	q := Quote{ID: "one", State: "new", Amount: 1000, Expires: time.Now().Add(time.Minute).Unix()}
	if err := store.Create(q); err != nil {
		t.Fatal(err)
	}
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, fresh, err := store.Claim(q.ID, q.Amount, time.Now())
			if err == nil && fresh {
				claimed.Add(1)
			} else if !errors.Is(err, ErrUncertain) {
				t.Errorf("unexpected claim result: %v", err)
			}
		})
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatal("more than one request claimed quote")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := store.Claim(q.ID, q.Amount, time.Now()); !errors.Is(err, ErrUncertain) {
		t.Fatal("restart allowed an uncertain request to run twice")
	}
	if err := store.Issued(q.ID, "persisted-invoice", "hash", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	loaded, fresh, err := store.Claim(q.ID, q.Amount, time.Now())
	if err != nil || fresh || loaded.Invoice != "persisted-invoice" {
		t.Fatalf("issued invoice was not reused: %+v %v", loaded, err)
	}
	if err := store.Prune(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(q.ID); !errors.Is(err, ErrUnknownQuote) {
		t.Fatal("expired quote not pruned")
	}
}

func TestInvoiceValidation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	q := Quote{Amount: 123000, Metadata: `[["text/plain","Coffee"]]`}
	hash := sha256.Sum256([]byte(q.Metadata))
	result, err := signedInvoice(q.Amount, hash, now, 300*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidateInvoice(result, q, "mainnet", now, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		edit func(*WalletInvoice, *Quote)
		net  string
		at   time.Time
	}{
		{"wrong network", func(*WalletInvoice, *Quote) {}, "testnet", now},
		{"expired", func(*WalletInvoice, *Quote) {}, "mainnet", now.Add(time.Hour)},
		{"wrong metadata", func(_ *WalletInvoice, q *Quote) { q.Metadata += " " }, "mainnet", now},
		{"wrong amount", func(_ *WalletInvoice, q *Quote) { q.Amount++ }, "mainnet", now},
		{"wrong response hash", func(r *WalletInvoice, _ *Quote) { r.PaymentHash = strings.Repeat("0", 64) }, "mainnet", now},
		{"tampered invoice", func(r *WalletInvoice, _ *Quote) { r.Invoice += "q" }, "mainnet", now},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, quote := result, q
			tt.edit(&r, &quote)
			if _, _, err := ValidateInvoice(r, quote, tt.net, tt.at, 300*time.Second); err == nil {
				t.Fatal("invalid invoice accepted")
			}
		})
	}
}

func TestPruneReclaimsUnusedQuotesAndRetainsInvoiceHistory(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "quotes.db"), 5)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().Truncate(time.Second)
	for _, q := range []Quote{
		{ID: "a", State: "new", Expires: now.Unix()},
		{ID: "b", State: "new", Expires: now.Unix() - 1},
		{ID: "c", State: "new", Expires: now.Unix() + 1},
		{ID: "d", State: "creating", Expires: now.Unix() - 1},
		{ID: "e", State: "issued", Expires: now.Unix() - 1, InvoiceEnd: now.Add(time.Minute).Unix()},
	} {
		if err := store.Create(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := store.Get(id); !errors.Is(err, ErrUnknownQuote) {
			t.Fatalf("expired unused quote %s was not removed: %v", id, err)
		}
	}
	for _, id := range []string{"c", "d", "e"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("quote %s was not retained: %v", id, err)
		}
	}
	if err := store.Create(Quote{ID: "f", State: "new", Expires: now.Unix() + 1}); err != nil {
		t.Fatalf("pruning did not reclaim capacity: %v", err)
	}
}

func TestStatusUsesSettlementNotPreimage(t *testing.T) {
	wallet := &fakeWallet{status: WalletStatus{Type: "incoming", Amount: 5000000}}
	_, handler := newTestAPI(t, wallet)
	callback, _ := newQuote(t, handler)
	getJSON(t, handler, callback+"?amount=5000000")
	data := getJSON(t, handler, callback+"/status")
	if stringField(t, data, "state") != "pending" {
		t.Fatal("unsettled invoice reported paid")
	}
	wallet.status.State = "expired"
	data = getJSON(t, handler, callback+"/status")
	if stringField(t, data, "state") != "expired" {
		t.Fatal("provider-reported expiry was not returned before local expiry")
	}
	wallet.status.State = ""
	wallet.status.SettledAt = time.Now().Unix()
	data = getJSON(t, handler, callback+"/status")
	if stringField(t, data, "state") != "settled" {
		t.Fatal("LNbits settled_at compatibility failed")
	}
	wallet.status.Amount++
	if data := getJSON(t, handler, callback+"/status"); stringField(t, data, "status") != "ERROR" {
		t.Fatal("inconsistent wallet status accepted")
	}
}

func TestQuoteRateLimitLeavesInvoiceCallbackAvailable(t *testing.T) {
	_, handler := newTestAPI(t, &fakeWallet{})
	callback, _ := newQuote(t, handler)
	for range 9 {
		newQuote(t, handler)
	}
	data := getJSON(t, handler, "/lnurl/coffee")
	if stringField(t, data, "reason") != "too many quote requests; try again later" {
		t.Fatal("quote-specific limit not enforced")
	}
	data = getJSON(t, handler, callback+"?amount=5000000")
	if stringField(t, data, "pr") == "" {
		t.Fatal("quote-specific limit blocked an existing invoice callback")
	}
}

func TestUnavailableDependenciesAndRateLimit(t *testing.T) {
	t.Run("offline wallet", func(t *testing.T) {
		api, _ := newTestAPI(t, &fakeWallet{offline: true})
		handler := api.Handler()
		data := getJSON(t, handler, "/lnurl/coffee")
		if stringField(t, data, "status") != "ERROR" {
			t.Fatal("offline wallet returned metadata")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatal("offline wallet was ready")
		}
	})
	t.Run("rate source failure", func(t *testing.T) {
		api, _ := newTestAPI(t, &fakeWallet{})
		api.rates = fixedRates{err: errors.New("rate unavailable")}
		data := getJSON(t, api.Handler(), "/lnurl/coffee")
		if stringField(t, data, "status") != "ERROR" {
			t.Fatal("rate failure returned metadata")
		}
	})
	t.Run("rate limiting", func(t *testing.T) {
		api, _ := newTestAPI(t, &fakeWallet{})
		api.cfg.RequestsPerSecond, api.cfg.Burst = 0.00001, 1
		handler := api.Handler()
		newQuote(t, handler)
		data := getJSON(t, handler, "/lnurl/coffee")
		if stringField(t, data, "status") != "ERROR" {
			t.Fatal("rate limit not enforced")
		}
	})
}
