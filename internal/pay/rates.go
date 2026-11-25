package pay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/currency"
)

const coinGeckoURL = "https://api.coingecko.com/api/v3/simple/price"

type PriceSource interface {
	FiatPerBTC(context.Context, string) (*big.Rat, error)
}

type cachedRate struct {
	value   *big.Rat
	updated time.Time
	fetched time.Time
}

type Rates struct {
	mu     sync.Mutex
	client *http.Client
	url    string
	maxAge time.Duration
	rates  map[string]cachedRate
}

func NewRates(maxAge time.Duration) *Rates {
	return &Rates{
		client: &http.Client{Timeout: 5 * time.Second},
		url:    coinGeckoURL, maxAge: maxAge, rates: make(map[string]cachedRate),
	}
}

func (r *Rates) FiatPerBTC(ctx context.Context, code string) (*big.Rat, error) {
	code = strings.ToLower(code)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cached := r.rates[code]
	if cached.value != nil && now.Sub(cached.fetched) < 30*time.Second && now.Sub(cached.updated) <= r.maxAge {
		return new(big.Rat).Set(cached.value), nil
	}
	endpoint, err := url.Parse(r.url)
	if err != nil {
		return nil, fmt.Errorf("parse exchange-rate URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("ids", "bitcoin")
	query.Set("vs_currencies", code)
	query.Set("include_last_updated_at", "true")
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create exchange-rate request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch exchange rate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange-rate provider returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Bitcoin map[string]json.Number `json:"bitcoin"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode exchange rate: %w", err)
	}
	value, ok := new(big.Rat).SetString(string(body.Bitcoin[code]))
	if !ok || value.Sign() <= 0 {
		return nil, errors.New("exchange-rate provider returned an invalid or unsupported currency price")
	}
	updatedNumber, ok := body.Bitcoin["last_updated_at"]
	if !ok {
		return nil, errors.New("exchange-rate provider omitted its update timestamp")
	}
	updatedUnix, err := updatedNumber.Int64()
	if err != nil {
		return nil, errors.New("exchange-rate provider returned an invalid update timestamp")
	}
	now = time.Now()
	updated := time.Unix(updatedUnix, 0)
	if now.Sub(updated) > r.maxAge || updated.After(now.Add(30*time.Second)) {
		return nil, errors.New("exchange-rate provider returned a stale or future timestamp")
	}
	r.rates[code] = cachedRate{value: value, updated: updated, fetched: now}
	return new(big.Rat).Set(value), nil
}

func QuoteMSats(code string, minorUnits int64, fiatPerBTC *big.Rat) (int64, error) {
	unit, err := currency.ParseISO(code)
	if err != nil || unit == currency.XXX || minorUnits <= 0 || fiatPerBTC == nil || fiatPerBTC.Sign() <= 0 {
		return 0, errors.New("valid ISO currency, positive minor-unit price, and exchange rate required")
	}
	scale, _ := currency.Standard.Rounding(unit)
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	sats := new(big.Rat).Quo(
		new(big.Rat).Mul(new(big.Rat).SetInt64(minorUnits), new(big.Rat).SetInt64(100000000)),
		new(big.Rat).Mul(new(big.Rat).SetInt(factor), fiatPerBTC),
	)
	whole, rem := new(big.Int).QuoRem(sats.Num(), sats.Denom(), new(big.Int))
	if rem.Sign() != 0 {
		whole.Add(whole, big.NewInt(1))
	}
	msats := whole.Mul(whole, big.NewInt(1000))
	if !msats.IsInt64() || msats.Sign() <= 0 || msats.Cmp(big.NewInt(2100000000000000000)) > 0 {
		return 0, errors.New("quoted amount exceeds the supported Bitcoin supply")
	}
	return msats.Int64(), nil
}
