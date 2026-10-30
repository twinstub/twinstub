package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func signed(t *testing.T, srv *httptest.Server, body string) int {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("whsec_test"))
	mac.Write([]byte(ts + "." + body))
	req, _ := http.NewRequest("POST", srv.URL+"/webhooks/acme", strings.NewReader(body))
	req.Header.Set("Acme-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestPaymentActivatesSubscription(t *testing.T) {
	srv := httptest.NewServer(NewService("http://unused", "whsec_test").Handler())
	defer srv.Close()

	for _, body := range []string{
		`{"id": "evt_1", "type": "payment.pending", "data": {"payment_id": "pay_1", "reference": "u1", "amount": 900}}`,
		`{"id": "evt_2", "type": "payment.completed", "data": {"payment_id": "pay_1", "reference": "u1", "amount": 900}}`,
	} {
		if code := signed(t, srv, body); code != 200 {
			t.Fatalf("webhook answered %d", code)
		}
	}

	resp, err := http.Get(srv.URL + "/accounts/u1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Account Account `json:"account"`
		Access  bool    `json:"access"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Access || out.Account.Credits != 900 {
		t.Errorf("account = %+v, access %v", out.Account, out.Access)
	}
}

func TestRejectsUnsignedWebhook(t *testing.T) {
	srv := httptest.NewServer(NewService("http://unused", "whsec_test").Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/webhooks/acme", "application/json", strings.NewReader(`{"id": "evt_1"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("unsigned webhook answered %d, want 400", resp.StatusCode)
	}
}
