package pay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const coinGeckoURL = "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=eur&include_last_updated_at=true"

type PriceSource interface {
	EURPerBTC(context.Context) (*big.Rat, error)
}

type Rates struct {
	mu      sync.Mutex
	client  *http.Client
	url     string
	maxAge  time.Duration
	value   *big.Rat
	updated time.Time
	fetched time.Time
}

func NewRates(maxAge time.Duration) *Rates {
	return &Rates{
		client: &http.Client{Timeout: 5 * time.Second},
		url:    coinGeckoURL, maxAge: maxAge,
	}
}

func (r *Rates) EURPerBTC(ctx context.Context) (*big.Rat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.value != nil && now.Sub(r.fetched) < 30*time.Second && now.Sub(r.updated) <= r.maxAge {
		return new(big.Rat).Set(r.value), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
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
		Bitcoin struct {
			EUR     json.Number `json:"eur"`
			Updated int64       `json:"last_updated_at"`
		} `json:"bitcoin"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode exchange rate: %w", err)
	}
	value, ok := new(big.Rat).SetString(string(body.Bitcoin.EUR))
	if !ok || value.Sign() <= 0 {
		return nil, errors.New("exchange-rate provider returned an invalid price")
	}
	now = time.Now()
	updated := time.Unix(body.Bitcoin.Updated, 0)
	if now.Sub(updated) > r.maxAge || updated.After(now.Add(30*time.Second)) {
		return nil, errors.New("exchange-rate provider returned a stale or future timestamp")
	}
	r.value, r.updated, r.fetched = value, updated, now
	return new(big.Rat).Set(value), nil
}

func QuoteMSats(cents int64, eurPerBTC *big.Rat) (int64, error) {
	if cents <= 0 || eurPerBTC == nil || eurPerBTC.Sign() <= 0 {
		return 0, errors.New("positive EUR cents and exchange rate required")
	}
	sats := new(big.Rat).Quo(
		new(big.Rat).Mul(new(big.Rat).SetInt64(cents), big.NewRat(1000000, 1)),
		eurPerBTC,
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
