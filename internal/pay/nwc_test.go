package pay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
	"github.com/nbd-wtf/go-nostr/nip44"
)

func TestNWCRelayRoundTripAndReconnect(t *testing.T) {
	for _, scheme := range []string{"nip44_v2", "nip04"} {
		t.Run(scheme, func(t *testing.T) {
			providerSecret, clientSecret := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
			providerPublic, _ := nostr.GetPublicKey(providerSecret)
			clientPublic, _ := nostr.GetPublicKey(clientSecret)
			shared44, err := nip44.GenerateConversationKey(clientPublic, providerSecret)
			if err != nil {
				t.Fatal(err)
			}
			shared04, err := nip04.ComputeSharedSecret(clientPublic, providerSecret)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte("exact LNURL metadata"))
			invoice, err := signedInvoice(100000, hash, time.Now(), 300*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			connections := make(chan *websocket.Conn, 8)
			failures := make(chan error, 16)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					failures <- err
					return
				}
				defer conn.CloseNow()
				conn.SetReadLimit(128 << 10)
				connections <- conn
				var subscription string
				write := func(value any) error {
					body, err := json.Marshal(value)
					if err != nil {
						return err
					}
					ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
					defer cancel()
					return conn.Write(ctx, websocket.MessageText, body)
				}
				for {
					_, body, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					var message []json.RawMessage
					if err := json.Unmarshal(body, &message); err != nil || len(message) < 2 {
						failures <- errors.New("invalid relay message")
						return
					}
					var kind string
					json.Unmarshal(message[0], &kind)
					switch kind {
					case "REQ":
						json.Unmarshal(message[1], &subscription)
						info := nostr.Event{
							Kind: 13194, CreatedAt: nostr.Now(),
							Content: "make_invoice lookup_invoice",
							Tags:    nostr.Tags{{"encryption", scheme}},
						}
						if err := info.Sign(providerSecret); err != nil {
							failures <- err
							return
						}
						if err := write([]any{"EVENT", subscription, info}); err != nil {
							return
						}
						if err := write([]any{"EOSE", subscription}); err != nil {
							return
						}
					case "EVENT":
						var event nostr.Event
						if err := json.Unmarshal(message[1], &event); err != nil {
							failures <- err
							return
						}
						valid, err := event.CheckSignature()
						if err != nil || !valid || event.PubKey != clientPublic ||
							singleTag(event.Tags, "p") != providerPublic ||
							singleTag(event.Tags, "encryption") != scheme {
							failures <- errors.New("request signature, destination or encryption incorrect")
							return
						}
						var plain string
						if scheme == "nip44_v2" {
							plain, err = nip44.Decrypt(event.Content, shared44)
						} else {
							plain, err = nip04.Decrypt(event.Content, shared04)
						}
						if err != nil {
							failures <- err
							return
						}
						var request struct {
							Method string `json:"method"`
							Params struct {
								Amount int64  `json:"amount"`
								Hash   string `json:"description_hash"`
								Expiry int    `json:"expiry"`
							} `json:"params"`
						}
						if err := json.Unmarshal([]byte(plain), &request); err != nil ||
							request.Method != "make_invoice" || request.Params.Amount != 100000 ||
							request.Params.Hash != hex.EncodeToString(hash[:]) || request.Params.Expiry != 300 {
							failures <- errors.New("request does not preserve invoice parameters")
							return
						}
						if err := write([]any{"OK", event.ID, true, ""}); err != nil {
							return
						}
						payload, _ := json.Marshal(map[string]any{"result_type": "make_invoice", "result": invoice})
						var ciphertext string
						if scheme == "nip44_v2" {
							ciphertext, err = nip44.Encrypt(string(payload), shared44)
						} else {
							ciphertext, err = nip04.Encrypt(string(payload), shared04)
						}
						if err != nil {
							failures <- err
							return
						}
						// A genuine provider response to another request must be ignored.
						for _, requestID := range []string{strings.Repeat("0", 64), event.ID} {
							response := nostr.Event{
								Kind: 23195, CreatedAt: nostr.Now(), Content: ciphertext,
								Tags: nostr.Tags{{"p", clientPublic}, {"e", requestID}, {"encryption", scheme}},
							}
							if err := response.Sign(providerSecret); err != nil {
								failures <- err
								return
							}
							if err := write([]any{"EVENT", subscription, response}); err != nil {
								return
							}
						}
					}
				}
			}))
			defer server.Close()
			pairing := "nostr+walletconnect://" + providerPublic + "?relay=" +
				url.QueryEscape("ws"+strings.TrimPrefix(server.URL, "http")) + "&secret=" + clientSecret
			client, err := parseNWC(pairing, true, testLogger())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); client.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("NWC reconnect loop did not stop")
				}
			}()
			waitReady := func() {
				t.Helper()
				timeout := time.After(5 * time.Second)
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for !client.Ready() {
					select {
					case err := <-failures:
						t.Fatal(err)
					case <-timeout:
						t.Fatal("NWC did not become ready")
					case <-ticker.C:
					}
				}
			}
			request := func() {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				got, err := client.MakeInvoice(ctx, 100000, hex.EncodeToString(hash[:]), 300)
				if err != nil || got != invoice {
					t.Fatalf("NWC round trip failed: %v", err)
				}
			}
			waitReady()
			request()
			first := <-connections
			first.CloseNow()
			select {
			case <-connections:
			case <-time.After(5 * time.Second):
				t.Fatal("NWC did not reconnect")
			}
			waitReady()
			request()
			select {
			case err := <-failures:
				t.Fatal(err)
			default:
			}
		})
	}
}

func TestNWCAuthenticationAndLegacyOptIn(t *testing.T) {
	secret, providerSecret := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	provider, _ := nostr.GetPublicKey(providerSecret)
	raw := "nostr+walletconnect://" + provider + "?relay=wss://relay.example.com&secret=" + secret
	client, err := parseNWC(raw, false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	info := nostr.Event{Kind: 13194, PubKey: provider, Content: "make_invoice lookup_invoice"}
	if _, err := client.infoScheme(&info); err == nil {
		t.Fatal("legacy NIP-04 enabled without opt-in")
	}
	info.Tags = nostr.Tags{{"encryption", "nip04 nip44_v2"}}
	if scheme, err := client.infoScheme(&info); err != nil || scheme != "nip44_v2" {
		t.Fatal("NIP-44 was not preferred")
	}
	client.legacy = true
	info.Tags = nil
	if scheme, err := client.infoScheme(&info); err != nil || scheme != "nip04" {
		t.Fatal("legacy provider without encryption tag not supported")
	}
	event := nostr.Event{Kind: 23195, CreatedAt: nostr.Now(), Content: "payload", Tags: nostr.Tags{}}
	if err := event.Sign(providerSecret); err != nil {
		t.Fatal(err)
	}
	if !client.validAuthor(&event) {
		t.Fatal("valid signed response rejected")
	}
	event.Content = "changed"
	if client.validAuthor(&event) {
		t.Fatal("tampered response accepted")
	}
	if err := event.Sign(secret); err != nil {
		t.Fatal(err)
	}
	if client.validAuthor(&event) {
		t.Fatal("response from wrong author accepted")
	}
	for _, content := range []string{"", "a?iv=b", "AAAA?iv=AAAA"} {
		if _, err := client.decrypt("nip04", content); err == nil {
			t.Fatal("malformed legacy ciphertext accepted")
		}
	}
	for _, invalid := range []string{
		strings.Replace(raw, "wss://", "ws://", 1),
		strings.Replace(raw, secret, strings.Repeat("0", 64), 1),
		raw + "&secret=" + secret,
	} {
		if _, err := parseNWC(invalid, false, testLogger()); err == nil ||
			strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), raw) {
			t.Fatal("invalid pairing accepted or error exposed credential")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.call(ctx, "pay_invoice", nil, nil); err == nil {
		t.Fatal("spending method was permitted")
	}
}
