package pay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrUnknownQuote = errors.New("unknown quote")
	ErrExpiredQuote = errors.New("quote expired; scan the QR again")
	ErrAmount       = errors.New("amount does not match the quote")
	ErrUncertain    = errors.New("invoice request is already in progress or its outcome is unknown; scan the QR again")
	ErrCapacity     = errors.New("quote storage is full")
)

type Quote struct {
	ID          string `json:"id"`
	Product     string `json:"product"`
	Wallet      string `json:"wallet"`
	Amount      int64  `json:"amount"`
	Metadata    string `json:"metadata"`
	Expires     int64  `json:"expires"`
	State       string `json:"state"`
	Invoice     string `json:"invoice,omitempty"`
	PaymentHash string `json:"payment_hash,omitempty"`
	InvoiceEnd  int64  `json:"invoice_end,omitempty"`
}

type Store struct {
	db  *bolt.DB
	max int
}

var quotesBucket = []byte("quotes-v1")

const (
	QuotePruneInterval   = time.Minute
	issuedQuoteRetention = time.Hour
)

func OpenStore(path string, max int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(quotesBucket)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, max: max}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func saveQuote(b *bolt.Bucket, q Quote) error {
	data, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return b.Put([]byte(q.ID), data)
}

func (s *Store) Create(q Quote) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(quotesBucket)
		if b.Stats().KeyN >= s.max {
			return ErrCapacity
		}
		if b.Get([]byte(q.ID)) != nil {
			return errors.New("duplicate quote ID")
		}
		return saveQuote(b, q)
	})
}

func (s *Store) Get(id string) (Quote, error) {
	var q Quote
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(quotesBucket).Get([]byte(id))
		if data == nil {
			return ErrUnknownQuote
		}
		return json.Unmarshal(data, &q)
	})
	return q, err
}

func (s *Store) Claim(id string, amount int64, now time.Time) (Quote, bool, error) {
	var q Quote
	fresh := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(quotesBucket)
		data := b.Get([]byte(id))
		if data == nil {
			return ErrUnknownQuote
		}
		if err := json.Unmarshal(data, &q); err != nil {
			return err
		}
		if now.Unix() >= q.Expires || (q.Invoice != "" && now.Unix() >= q.InvoiceEnd) {
			return ErrExpiredQuote
		}
		if amount != q.Amount {
			return ErrAmount
		}
		if q.State == "issued" {
			return nil
		}
		if q.State != "new" {
			return ErrUncertain
		}
		q.State, fresh = "creating", true
		return saveQuote(b, q)
	})
	return q, fresh, err
}

func (s *Store) Issued(id, invoice, hash string, expiry time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(quotesBucket)
		data := b.Get([]byte(id))
		if data == nil {
			return ErrUnknownQuote
		}
		var q Quote
		if err := json.Unmarshal(data, &q); err != nil {
			return err
		}
		if q.State != "creating" {
			return errors.New("invalid invoice state transition")
		}
		q.State, q.Invoice, q.PaymentHash, q.InvoiceEnd = "issued", invoice, hash, expiry.Unix()
		return saveQuote(b, q)
	})
}

func (s *Store) Prune(now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket(quotesBucket).Cursor()
		for k, v := c.First(); k != nil; {
			var q Quote
			if err := json.Unmarshal(v, &q); err != nil {
				return err
			}
			expiry := q.Expires
			if q.InvoiceEnd > expiry {
				expiry = q.InvoiceEnd
			}
			if q.State != "new" {
				expiry += int64(issuedQuoteRetention / time.Second)
			}
			if now.Unix() >= expiry {
				// Seek back to the deleted key to avoid skipping its successor.
				key := bytes.Clone(k)
				if err := c.Delete(); err != nil {
					return err
				}
				k, v = c.Seek(key)
			} else {
				k, v = c.Next()
			}
		}
		return nil
	})
}
