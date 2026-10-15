package main

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func testSeller(t *testing.T, mux *http.ServeMux) *seller {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &seller{
		base:   srv.URL,
		id:     "agent",
		key:    ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
		client: srv.Client(),
	}
}

// A session that has lapsed must not fail the trade: the first authenticated
// call is refused, the seller re-authenticates, and the call succeeds.
func TestCallAuthedReauthenticatesWhenSessionLapses(t *testing.T) {
	var verifies, accepts int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/challenge", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"challengeId": "c", "nonce": "n"})
	})
	mux.HandleFunc("/v1/auth/verify", func(w http.ResponseWriter, r *http.Request) {
		token := "expired"
		if atomic.AddInt64(&verifies, 1) > 1 {
			token = "fresh"
		}
		writeJSON(w, http.StatusOK, map[string]any{"token": token})
	})
	mux.HandleFunc("/v1/trades/t1/accept", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&accepts, 1)
		if r.Header.Get("authorization") == "Bearer expired" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "token is expired"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
	})

	s := testSeller(t, mux)
	if err := s.authenticate(); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	out, err := s.callAuthed("/v1/trades/t1/accept", map[string]any{}, "POST")
	if err != nil {
		t.Fatalf("callAuthed after expiry: %v", err)
	}
	if out["accepted"] != true {
		t.Fatalf("accepted = %v, want true", out["accepted"])
	}
	if got := atomic.LoadInt64(&verifies); got != 2 {
		t.Fatalf("auth verifies = %d, want 2 (one initial, one renewal)", got)
	}
	if got := atomic.LoadInt64(&accepts); got != 2 {
		t.Fatalf("accept attempts = %d, want 2 (refused then retried)", got)
	}
}

// Only a 401 is a session lapse. Any other refusal must be returned as-is; a
// blanket retry would re-authenticate on a validation error and hide it.
func TestCallAuthedDoesNotRetryOtherRefusals(t *testing.T) {
	var verifies int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/challenge", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"challengeId": "c", "nonce": "n"})
	})
	mux.HandleFunc("/v1/auth/verify", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&verifies, 1)
		writeJSON(w, http.StatusOK, map[string]any{"token": "tok"})
	})
	mux.HandleFunc("/v1/trades/t1/accept", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "trade is not open"})
	})

	s := testSeller(t, mux)
	if err := s.authenticate(); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := s.callAuthed("/v1/trades/t1/accept", map[string]any{}, "POST"); err == nil {
		t.Fatal("callAuthed succeeded on a 409, want the refusal")
	}
	if got := atomic.LoadInt64(&verifies); got != 1 {
		t.Fatalf("auth verifies = %d, want 1 (no renewal on a non-401)", got)
	}
}
