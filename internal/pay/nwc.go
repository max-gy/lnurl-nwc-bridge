package pay

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
	"github.com/nbd-wtf/go-nostr/nip44"
)

var ErrWalletOffline = errors.New("wallet connection is not ready")

type Wallet interface {
	Ready() bool
	MakeInvoice(context.Context, int64, string, int) (WalletInvoice, error)
	LookupInvoice(context.Context, string) (WalletStatus, error)
}

type WalletInvoice struct {
	Invoice     string `json:"invoice"`
	PaymentHash string `json:"payment_hash"`
	Amount      int64  `json:"amount"`
}

type WalletStatus struct {
	Type        string `json:"type"`
	State       string `json:"state"`
	PaymentHash string `json:"payment_hash"`
	Amount      int64  `json:"amount"`
	SettledAt   int64  `json:"settled_at"`
}

type nwcSession struct {
	conn   *websocket.Conn
	ctx    context.Context
	scheme string
}

type nwcPending struct {
	response chan *nostr.Event
	rejected chan struct{}
}

type NWC struct {
	mu       sync.Mutex
	secret   string
	public   string
	provider string
	relayURL string
	legacy   bool
	shared04 []byte
	shared44 [32]byte
	session  *nwcSession
	pending  map[string]*nwcPending
	log      *slog.Logger
}

func NewNWC(cfg WalletConfig, log *slog.Logger) (*NWC, error) {
	f, err := os.Open(cfg.URIFile)
	if err != nil {
		return nil, errors.New("cannot open NWC credential file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return nil, errors.New("NWC credential must be a regular file of at most 8192 bytes")
	}
	data := make([]byte, info.Size())
	if _, err := f.ReadAt(data, 0); err != nil {
		return nil, errors.New("cannot read NWC credential file")
	}
	return parseNWC(strings.TrimSpace(string(data)), cfg.AllowLegacyNIP04, log)
}

func parseNWC(raw string, legacy bool, log *slog.Logger) (*NWC, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "nostr+walletconnect" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.Fragment != "" {
		return nil, errors.New("invalid NWC pairing URI")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query["secret"]) != 1 || len(query["relay"]) != 1 {
		return nil, errors.New("NWC URI requires one secret and one relay")
	}
	secret := query.Get("secret")
	sk, err := hex.DecodeString(secret)
	if err != nil || len(sk) != 32 || new(big.Int).SetBytes(sk).Sign() == 0 ||
		new(big.Int).SetBytes(sk).Cmp(btcec.S256().N) >= 0 {
		return nil, errors.New("NWC secret must be a valid 32-byte private key in hex")
	}
	provider := u.Host
	pk, err := hex.DecodeString(provider)
	if err != nil || len(pk) != 32 {
		return nil, errors.New("NWC provider must be a 32-byte public key in hex")
	}
	relay, err := url.Parse(query.Get("relay"))
	if err != nil || relay.Hostname() == "" || relay.User != nil ||
		relay.RawQuery != "" || relay.Fragment != "" {
		return nil, errors.New("invalid relay URL")
	}
	loopback := relay.Hostname() == "localhost"
	if ip := net.ParseIP(relay.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if relay.Scheme != "wss" && !(relay.Scheme == "ws" && loopback) {
		return nil, errors.New("relay must use WSS; WS is allowed only on loopback for local testing")
	}
	public, err := nostr.GetPublicKey(secret)
	if err != nil {
		return nil, errors.New("invalid NWC client key")
	}
	provider = strings.ToLower(provider)
	shared04, err := nip04.ComputeSharedSecret(provider, secret)
	if err != nil {
		return nil, errors.New("invalid NWC provider key")
	}
	shared44, err := nip44.GenerateConversationKey(provider, secret)
	if err != nil {
		return nil, errors.New("cannot derive NWC encryption key")
	}
	return &NWC{
		secret: secret, public: public, provider: provider, relayURL: relay.String(),
		legacy: legacy, shared04: shared04, shared44: shared44,
		pending: make(map[string]*nwcPending), log: log,
	}, nil
}

func (n *NWC) Ready() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.session != nil && n.session.ctx.Err() == nil
}

func (n *NWC) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		if err := n.runSession(ctx); err != nil && ctx.Err() == nil {
			// Relay errors can contain remote-controlled text; never log raw payloads.
			n.log.Warn("NWC session unavailable; reconnecting")
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (n *NWC) runSession(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, n.relayURL, nil)
	cancel()
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return err
	}
	conn.SetReadLimit(128 << 10)
	sessionCtx, cancelSession := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancelSession()
		conn.CloseNow()
		workers.Wait()
		n.mu.Lock()
		n.session = nil
		n.mu.Unlock()
	}()
	since := nostr.Now() - 30
	request, err := json.Marshal([]any{"REQ", "nwc",
		nostr.Filter{Kinds: []int{13194}, Authors: []string{n.provider}, Limit: 1},
		nostr.Filter{Kinds: []int{23195}, Authors: []string{n.provider}, Tags: nostr.TagMap{"p": []string{n.public}}, Since: &since},
	})
	if err != nil {
		return err
	}
	writeCtx, cancelWrite := context.WithTimeout(sessionCtx, 5*time.Second)
	err = conn.Write(writeCtx, websocket.MessageText, request)
	cancelWrite()
	if err != nil {
		return err
	}
	messages := make(chan []json.RawMessage, 32)
	connectionErrors := make(chan error, 2)
	workers.Go(func() {
		for {
			kind, data, err := conn.Read(sessionCtx)
			if err != nil {
				connectionErrors <- err
				return
			}
			var message []json.RawMessage
			if kind != websocket.MessageText || json.Unmarshal(data, &message) != nil || len(message) < 1 {
				connectionErrors <- errors.New("invalid relay message")
				return
			}
			select {
			case messages <- message:
			case <-sessionCtx.Done():
				return
			}
		}
	})
	workers.Go(func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(sessionCtx, 5*time.Second)
				err := conn.Ping(pingCtx)
				cancel()
				if err != nil {
					connectionErrors <- err
					return
				}
			}
		}
	})
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	var infoTimeout <-chan time.Time = deadline.C
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-connectionErrors:
			return err
		case <-infoTimeout:
			return errors.New("wallet info event unavailable")
		case message := <-messages:
			var kind string
			if err := json.Unmarshal(message[0], &kind); err != nil {
				return errors.New("invalid relay message kind")
			}
			switch kind {
			case "CLOSED", "AUTH":
				return errors.New("relay closed the subscription or requires unsupported authentication")
			case "NOTICE":
				n.log.Warn("Nostr relay sent a notice")
				continue
			case "OK":
				if len(message) < 3 {
					return errors.New("invalid relay acknowledgement")
				}
				var id string
				var accepted bool
				if json.Unmarshal(message[1], &id) != nil || json.Unmarshal(message[2], &accepted) != nil {
					return errors.New("invalid relay acknowledgement fields")
				}
				n.mu.Lock()
				pending := n.pending[id]
				n.mu.Unlock()
				if pending != nil && !accepted {
					select {
					case pending.rejected <- struct{}{}:
					default:
					}
				}
				continue
			case "EVENT":
			default:
				continue
			}
			var subscription string
			var event nostr.Event
			if len(message) != 3 || json.Unmarshal(message[1], &subscription) != nil ||
				subscription != "nwc" || json.Unmarshal(message[2], &event) != nil {
				return errors.New("invalid relay event envelope")
			}
			if !n.validAuthor(&event) {
				n.log.Warn("Discarded unauthenticated NWC event")
				continue
			}
			if event.Kind == 13194 {
				scheme, err := n.infoScheme(&event)
				if err != nil {
					n.log.Error("Wallet has no compatible NWC capabilities or encryption")
					return err
				}
				n.mu.Lock()
				n.session = &nwcSession{conn: conn, ctx: sessionCtx, scheme: scheme}
				n.mu.Unlock()
				if infoTimeout != nil {
					n.log.Info("NWC wallet connected", "encryption", scheme)
				}
				infoTimeout = nil
				deadline.Stop()
				continue
			}
			if event.Kind != 23195 || singleTag(event.Tags, "p") != n.public {
				continue
			}
			id := singleTag(event.Tags, "e")
			n.mu.Lock()
			pending := n.pending[id]
			n.mu.Unlock()
			if pending != nil {
				select {
				case pending.response <- &event:
				default:
				}
			}
		}
	}
}

func (n *NWC) validAuthor(event *nostr.Event) bool {
	if event == nil || event.PubKey != n.provider || len(event.Content) > 65536 ||
		event.ID != event.GetID() || event.CreatedAt > nostr.Now()+30 {
		return false
	}
	valid, err := event.CheckSignature()
	return err == nil && valid
}

func singleTag(tags nostr.Tags, name string) string {
	var value string
	count := 0
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			count++
			value = tag[1]
		}
	}
	if count != 1 {
		return ""
	}
	return value
}

func (n *NWC) infoScheme(event *nostr.Event) (string, error) {
	methods := strings.Fields(event.Content)
	hasMake, hasLookup := false, false
	for _, method := range methods {
		hasMake = hasMake || method == "make_invoice"
		hasLookup = hasLookup || method == "lookup_invoice"
	}
	if !hasMake || !hasLookup {
		return "", errors.New("wallet must support make_invoice and lookup_invoice")
	}
	encryption := singleTag(event.Tags, "encryption")
	for _, scheme := range strings.Fields(encryption) {
		if scheme == "nip44_v2" {
			return scheme, nil
		}
	}
	if n.legacy {
		if encryption == "" {
			return "nip04", nil
		}
		for _, scheme := range strings.Fields(encryption) {
			if scheme == "nip04" {
				return scheme, nil
			}
		}
	}
	return "", errors.New("NIP-44 unavailable; legacy NIP-04 requires explicit opt-in")
}

func (n *NWC) encrypt(scheme, plaintext string) (string, error) {
	if scheme == "nip44_v2" {
		return nip44.Encrypt(plaintext, n.shared44)
	}
	return nip04.Encrypt(plaintext, n.shared04)
}

func (n *NWC) decrypt(scheme, content string) (string, error) {
	if scheme == "nip44_v2" {
		return nip44.Decrypt(content, n.shared44)
	}
	// The legacy library requires callers to validate CBC block and IV lengths.
	parts := strings.Split(content, "?iv=")
	if len(parts) != 2 {
		return "", errors.New("invalid NIP-04 payload")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil || len(ciphertext) == 0 || len(ciphertext)%16 != 0 {
		return "", errors.New("invalid NIP-04 ciphertext")
	}
	iv, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || len(iv) != 16 {
		return "", errors.New("invalid NIP-04 IV")
	}
	return nip04.Decrypt(content, n.shared04)
}

func (n *NWC) call(ctx context.Context, method string, params any, result any) error {
	if method != "make_invoice" && method != "lookup_invoice" {
		return errors.New("NWC method is not permitted")
	}
	n.mu.Lock()
	session := n.session
	n.mu.Unlock()
	if session == nil || session.ctx.Err() != nil {
		return ErrWalletOffline
	}
	request := struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}{method, params}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	content, err := n.encrypt(session.scheme, string(data))
	if err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("NWC requests require a deadline")
	}
	event := nostr.Event{
		Kind: 23194, CreatedAt: nostr.Now(), Content: content,
		Tags: nostr.Tags{
			{"p", n.provider}, {"encryption", session.scheme},
			{"expiration", fmt.Sprint(deadline.Unix())},
		},
	}
	if err := event.Sign(n.secret); err != nil {
		return err
	}
	pending := &nwcPending{
		response: make(chan *nostr.Event, 1),
		rejected: make(chan struct{}, 1),
	}
	n.mu.Lock()
	n.pending[event.ID] = pending
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.pending, event.ID)
		n.mu.Unlock()
	}()
	body, err := json.Marshal([]any{"EVENT", event})
	if err != nil {
		return err
	}
	if err := session.conn.Write(ctx, websocket.MessageText, body); err != nil {
		return errors.New("cannot publish NWC request")
	}
	select {
	case <-ctx.Done():
		return errors.New("NWC response timed out")
	case <-session.ctx.Done():
		return errors.New("NWC relay disconnected while awaiting response")
	case <-pending.rejected:
		return errors.New("relay rejected NWC request")
	case event := <-pending.response:
		scheme := singleTag(event.Tags, "encryption")
		if scheme != "" && scheme != session.scheme {
			return errors.New("unexpected NWC response encryption")
		}
		data, err := n.decrypt(session.scheme, event.Content)
		if err != nil {
			return errors.New("cannot decrypt NWC response")
		}
		var response struct {
			Type   string          `json:"result_type"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &response); err != nil || response.Type != method {
			return errors.New("invalid NWC response")
		}
		if response.Error != nil {
			return errors.New("wallet rejected the NWC request")
		}
		if len(response.Result) == 0 || string(response.Result) == "null" {
			return errors.New("NWC response has no result")
		}
		if err := json.Unmarshal(response.Result, result); err != nil {
			return errors.New("invalid NWC result")
		}
		return nil
	}
}

func (n *NWC) MakeInvoice(ctx context.Context, amount int64, descriptionHash string, expiry int) (WalletInvoice, error) {
	var result WalletInvoice
	params := struct {
		Amount          int64  `json:"amount"`
		DescriptionHash string `json:"description_hash"`
		Expiry          int    `json:"expiry"`
	}{amount, descriptionHash, expiry}
	err := n.call(ctx, "make_invoice", params, &result)
	return result, err
}

func (n *NWC) LookupInvoice(ctx context.Context, hash string) (WalletStatus, error) {
	var result WalletStatus
	err := n.call(ctx, "lookup_invoice", struct {
		Hash string `json:"payment_hash"`
	}{hash}, &result)
	return result, err
}
