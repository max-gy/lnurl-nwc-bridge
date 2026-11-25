package pay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

var quoteIDPattern = regexp.MustCompile(`^[0-9a-f]{48}$`)
var amountPattern = regexp.MustCompile(`^[0-9]{1,19}$`)

type API struct {
	cfg     Config
	store   *Store
	rates   PriceSource
	wallets map[string]Wallet
	log     *slog.Logger
}

func NewAPI(cfg Config, store *Store, prices PriceSource, wallets map[string]Wallet, log *slog.Logger) *API {
	return &API{cfg: cfg, store: store, rates: prices, wallets: wallets, log: log}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	quoteLimiter := a.quoteLimiter()
	mux.HandleFunc("GET /lnurl/{product}", func(w http.ResponseWriter, r *http.Request) {
		if !quoteLimiter.Allow() {
			a.fail(w, "too many quote requests; try again later", errors.New("quote request rate exceeded"))
			return
		}
		a.metadata(w, r)
	})
	mux.HandleFunc("GET /quote/{id}", a.invoice)
	mux.HandleFunc("GET /quote/{id}/status", a.status)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		for _, wallet := range a.wallets {
			if !wallet.Ready() {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "wallet unavailable"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	limiter := rate.NewLimiter(rate.Limit(a.cfg.RequestsPerSecond), a.cfg.Burst)
	inflight := make(chan struct{}, 32)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			mux.ServeHTTP(w, r)
			return
		}
		if len(r.RequestURI) > 2048 {
			a.fail(w, "request URI is too long", errors.New("oversized request URI"))
			return
		}
		if !limiter.Allow() {
			a.fail(w, "too many requests; try again later", errors.New("global request rate exceeded"))
			return
		}
		select {
		case inflight <- struct{}{}:
			defer func() { <-inflight }()
		default:
			a.fail(w, "service busy; try again later", errors.New("in-flight request limit exceeded"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *API) quoteLimiter() *rate.Limiter {
	// Budget for the longest-lived quotes even if every quote produces an invoice.
	maxLifetime := time.Duration(a.cfg.QuoteTTLSeconds+a.cfg.InvoiceTTLSeconds)*time.Second +
		2*a.cfg.Timeout() + invoiceClockSkew + issuedQuoteRetention + QuotePruneInterval
	burst := min(a.cfg.Burst, 10, max(1, a.store.max/4))
	storageRate := float64(max(1, a.store.max-burst)) / (2 * maxLifetime.Seconds())
	return rate.NewLimiter(rate.Limit(min(1, a.cfg.RequestsPerSecond/2, storageRate)), burst)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("Cannot write HTTP response")
	}
}

func (a *API) fail(w http.ResponseWriter, reason string, err error) {
	a.log.Warn("LNURL request failed", "reason", reason, "error", err)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ERROR", "reason": reason})
}

func (a *API) metadata(w http.ResponseWriter, r *http.Request) {
	p, ok := a.cfg.Products[r.PathValue("product")]
	if !ok {
		a.fail(w, "unknown product", errors.New("product not configured"))
		return
	}
	if !a.wallets[p.Wallet].Ready() {
		a.fail(w, "home wallet is temporarily unavailable", ErrWalletOffline)
		return
	}
	var amount, minAmount, maxAmount int64
	if p.Currency != "" {
		ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout())
		exchangeRate, err := a.rates.FiatPerBTC(ctx, p.Currency)
		cancel()
		if err != nil {
			a.fail(w, "fresh EUR exchange rate unavailable", err)
			return
		}
		amount, err = QuoteMSats(p.Currency, p.Price, exchangeRate)
		if err != nil {
			a.fail(w, "cannot quote this price", err)
			return
		}
		minAmount, maxAmount = amount, amount
	} else {
		minAmount, maxAmount = p.MinSats*1000, p.MaxSats*1000
	}
	rawMetadata, err := json.Marshal([][]string{{"text/plain", p.Description}})
	if err != nil {
		a.fail(w, "cannot encode product metadata", err)
		return
	}
	id := make([]byte, 24)
	if _, err := rand.Read(id); err != nil {
		a.fail(w, "cannot create quote", err)
		return
	}
	q := Quote{
		ID: hex.EncodeToString(id), Product: r.PathValue("product"),
		Wallet: p.Wallet, Amount: amount, MinAmount: minAmount, MaxAmount: maxAmount,
		Metadata: string(rawMetadata),
		Expires:  time.Now().Add(time.Duration(a.cfg.QuoteTTLSeconds) * time.Second).Unix(),
		State:    "new",
	}
	if err := a.store.Create(q); err != nil {
		a.fail(w, "quote service temporarily unavailable", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Tag         string `json:"tag"`
		Callback    string `json:"callback"`
		MinSendable int64  `json:"minSendable"`
		MaxSendable int64  `json:"maxSendable"`
		Metadata    string `json:"metadata"`
	}{
		Tag: "payRequest", Callback: strings.TrimSuffix(a.cfg.PublicURL, "/") + "/quote/" + q.ID,
		MinSendable: minAmount, MaxSendable: maxAmount, Metadata: q.Metadata,
	})
}

func (a *API) invoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	query, err := strconvAmount(r)
	if !quoteIDPattern.MatchString(id) || err != nil {
		a.fail(w, "invalid quote ID or amount", errors.New("malformed callback"))
		return
	}
	// Check connectivity before claiming, so a known outage does not consume the quote.
	existing, err := a.store.Get(id)
	if err != nil {
		a.quoteError(w, err)
		return
	}
	wallet, ok := a.wallets[existing.Wallet]
	if !ok {
		a.fail(w, "quote wallet is no longer configured; scan the QR again", errors.New("removed quote wallet"))
		return
	}
	if existing.State != "issued" && !wallet.Ready() {
		a.fail(w, "home wallet is temporarily unavailable", ErrWalletOffline)
		return
	}
	q, fresh, err := a.store.Claim(id, query, time.Now())
	if err != nil {
		a.quoteError(w, err)
		return
	}
	if fresh {
		hash := sha256.Sum256([]byte(q.Metadata))
		ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout())
		defer cancel()
		result, err := wallet.MakeInvoice(ctx, q.Amount, hex.EncodeToString(hash[:]), a.cfg.InvoiceTTLSeconds)
		if err != nil {
			a.fail(w, "invoice request failed or its outcome is unknown; scan the QR again", err)
			return
		}
		paymentHash, expiry, err := ValidateInvoice(result, q, a.cfg.Network, time.Now(), time.Duration(a.cfg.InvoiceTTLSeconds)*time.Second)
		if err != nil {
			a.fail(w, "home wallet returned an unsuitable invoice", err)
			return
		}
		if err := a.store.Issued(q.ID, result.Invoice, paymentHash, expiry); err != nil {
			a.fail(w, "cannot persist invoice; scan the QR again", err)
			return
		}
		q.Invoice = result.Invoice
	}
	writeJSON(w, http.StatusOK, struct {
		PR     string   `json:"pr"`
		Routes []string `json:"routes"`
	}{PR: q.Invoice, Routes: []string{}})
}

func strconvAmount(r *http.Request) (int64, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, errors.New("invalid callback query")
	}
	values := query["amount"]
	if len(values) != 1 || !amountPattern.MatchString(values[0]) {
		return 0, errors.New("one integer amount is required")
	}
	amount, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || amount <= 0 {
		return 0, errors.New("amount must be a positive millisatoshi integer")
	}
	return amount, nil
}

func (a *API) quoteError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnknownQuote) || errors.Is(err, ErrExpiredQuote) ||
		errors.Is(err, ErrAmount) || errors.Is(err, ErrUncertain) {
		a.fail(w, err.Error(), err)
		return
	}
	a.fail(w, "cannot read quote state", err)
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !quoteIDPattern.MatchString(id) {
		a.fail(w, "invalid quote ID", errors.New("malformed status request"))
		return
	}
	q, err := a.store.Get(id)
	if err != nil {
		a.quoteError(w, err)
		return
	}
	if q.State != "issued" {
		writeJSON(w, http.StatusOK, map[string]string{"state": q.State})
		return
	}
	wallet, ok := a.wallets[q.Wallet]
	if !ok {
		a.fail(w, "quote wallet is no longer configured", errors.New("removed quote wallet"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout())
	defer cancel()
	result, err := wallet.LookupInvoice(ctx, q.PaymentHash)
	if err != nil {
		a.fail(w, "cannot obtain payment status", err)
		return
	}
	if result.PaymentHash != q.PaymentHash || result.Amount != q.Amount || result.Type != "incoming" {
		a.fail(w, "wallet returned inconsistent payment status", errors.New("NWC lookup result mismatch"))
		return
	}
	state := "pending"
	switch {
	case result.State == "settled" || result.SettledAt > 0:
		state = "settled"
	case result.State == "expired" || time.Now().Unix() >= q.InvoiceEnd:
		state = "expired"
	case result.State == "accepted":
		state = "accepted"
	case result.State != "" && result.State != "pending":
		a.fail(w, "wallet returned an unexpected payment state", fmt.Errorf("unrecognized invoice state"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": state})
}
