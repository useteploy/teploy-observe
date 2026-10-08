package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOIDCSharedRefreshExpiresAfterInitiatingLoginCancels(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-r.Context().Done() }))
	defer server.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign([]byte(`{"sub":"synthetic"}`))
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 50 * time.Millisecond}
	ctx := oidcHTTPContext(context.WithValue(context.Background(), oauth2.HTTPClient, client))
	keys := oidc.NewRemoteKeySet(ctx, server.URL)
	first, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := keys.VerifySignature(first, jwt); err == nil {
		t.Fatal("stalled refresh succeeded")
	}
	// First caller cancellation must not leave the shared slot occupied forever.
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(60 * time.Millisecond)
		retry, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err = keys.VerifySignature(retry, jwt)
		stop()
		if err == nil {
			t.Fatal("stalled refresh succeeded")
		}
	}
	if calls.Load() < 2 {
		t.Fatal("canceled caller left refresh slot occupied")
	}
	if client.Timeout != 50*time.Millisecond {
		t.Fatal("configured client mutated")
	}
}

func TestOIDCClientHasFiniteWholeResponseDeadline(t *testing.T) {
	client := oidcHTTPContext(context.Background()).Value(oauth2.HTTPClient).(*http.Client)
	if client.Timeout != 10*time.Second {
		t.Fatalf("timeout %s", client.Timeout)
	}
}
