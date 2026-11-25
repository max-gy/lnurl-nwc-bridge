package pay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/zpay32"
)

const invoiceClockSkew = 30 * time.Second

func ValidateInvoice(result WalletInvoice, q Quote, network string, now time.Time, ttl time.Duration) (string, time.Time, error) {
	params := &chaincfg.MainNetParams
	switch network {
	case "testnet":
		params = &chaincfg.TestNet3Params
	case "regtest":
		params = &chaincfg.RegressionNetParams
	case "signet":
		params = &chaincfg.SigNetParams
	case "mainnet":
	default:
		return "", time.Time{}, errors.New("unsupported invoice network")
	}
	invoice, err := zpay32.Decode(result.Invoice, params, zpay32.WithErrorOnUnknownFeatureBit())
	if err != nil {
		return "", time.Time{}, errors.New("wallet returned an invalid invoice")
	}
	hash := sha256.Sum256([]byte(q.Metadata))
	if invoice.MilliSat == nil || int64(*invoice.MilliSat) != q.Amount ||
		invoice.DescriptionHash == nil || *invoice.DescriptionHash != hash ||
		invoice.PaymentHash == nil || invoice.Destination == nil {
		return "", time.Time{}, errors.New("invoice does not match the quote and metadata")
	}
	if result.Amount != q.Amount {
		return "", time.Time{}, errors.New("NWC result amount does not match the quote")
	}
	paymentHash := hex.EncodeToString(invoice.PaymentHash[:])
	if result.PaymentHash != paymentHash {
		return "", time.Time{}, errors.New("NWC payment hash does not match the invoice")
	}
	expiry := invoice.Timestamp.Add(invoice.Expiry())
	if invoice.Timestamp.After(now.Add(invoiceClockSkew)) || !expiry.After(now.Add(5*time.Second)) ||
		expiry.After(now.Add(ttl+invoiceClockSkew)) {
		return "", time.Time{}, errors.New("invoice expiry is outside the permitted window")
	}
	return paymentHash, expiry, nil
}
