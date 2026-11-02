package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestService(t *testing.T, dataFile string) *Service {
	t.Helper()
	svc, err := NewService("http://unused", "whsec_test", dataFile)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func accountOf(t *testing.T, srv *httptest.Server, user string) (Account, bool) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/accounts/" + user)
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
	return out.Account, out.Access
}

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
	data := filepath.Join(t.TempDir(), "accounts.json")
	srv := httptest.NewServer(newTestService(t, data).Handler())
	defer srv.Close()

	for _, body := range []string{
		`{"id": "evt_1", "type": "payment.pending", "data": {"payment_id": "pay_1", "reference": "u1", "amount": 900}}`,
		`{"id": "evt_2", "type": "payment.completed", "data": {"payment_id": "pay_1", "reference": "u1", "amount": 900}}`,
	} {
		if code := signed(t, srv, body); code != 200 {
			t.Fatalf("webhook answered %d", code)
		}
	}
	if a, access := accountOf(t, srv, "u1"); !access || a.Credits != 900 {
		t.Errorf("account = %+v, access %v", a, access)
	}

	// The account survives a restart.
	again := httptest.NewServer(newTestService(t, data).Handler())
	defer again.Close()
	if a, access := accountOf(t, again, "u1"); !access || a.Credits != 900 {
		t.Errorf("after restart: account = %+v, access %v", a, access)
	}
}

func TestFailedSaveAnswers503(t *testing.T) {
	svc := newTestService(t, filepath.Join(t.TempDir(), "missing-dir", "accounts.json"))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	completed := `{"id": "evt_2", "type": "payment.completed", "data": {"payment_id": "pay_1", "reference": "u1", "amount": 900}}`
	if code := signed(t, srv, completed); code != 503 {
		t.Fatalf("webhook with storage down answered %d, want 503", code)
	}
	if a, access := accountOf(t, srv, "u1"); access || a.Credits != 0 {
		t.Errorf("a failed save changed the account: %+v", a)
	}
}

func TestRejectsUnsignedWebhook(t *testing.T) {
	srv := httptest.NewServer(newTestService(t, filepath.Join(t.TempDir(), "accounts.json")).Handler())
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
