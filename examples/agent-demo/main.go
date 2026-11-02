// Command agent-demo is a small subscription service. Customers pay through
// Acme Pay; the service learns the outcome from Acme Pay's webhooks and
// grants access and credits when a payment completes. Accounts are kept in
// a JSON file so that a restart does not lose them.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Plan prices in cents.
var plans = map[string]int{"basic": 900, "pro": 2900}

type Account struct {
	User      string `json:"user"`
	Plan      string `json:"plan,omitempty"`
	Status    string `json:"status"`  // none, pending, active, revoked
	Credits   int    `json:"credits"` // cents paid and not reversed
	PaymentID string `json:"payment_id,omitempty"`
}

// Access reports whether the customer may use the paid plan.
func (a *Account) Access() bool { return a.Status == "active" }

type Service struct {
	acmeURL    string
	secret     string
	dataFile   string
	httpClient *http.Client

	mu       sync.Mutex
	accounts map[string]*Account
}

// NewService loads the accounts saved in dataFile, if it exists.
func NewService(acmeURL, secret, dataFile string) (*Service, error) {
	s := &Service{
		acmeURL:    strings.TrimRight(acmeURL, "/"),
		secret:     secret,
		dataFile:   dataFile,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		accounts:   map[string]*Account{},
	}
	data, err := os.ReadFile(dataFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.accounts); err != nil {
		return nil, fmt.Errorf("%s: %w", dataFile, err)
	}
	return s, nil
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /subscribe", s.subscribe)
	mux.HandleFunc("GET /accounts/{user}", s.account)
	mux.HandleFunc("POST /webhooks/acme", s.webhook)
	return mux
}

// get returns a copy of the user's account; put stores a changed copy once
// it is saved.
func (s *Service) get(user string) Account {
	if a, ok := s.accounts[user]; ok {
		return *a
	}
	return Account{User: user, Status: "none"}
}

func (s *Service) put(a Account) error {
	next := make(map[string]*Account, len(s.accounts)+1)
	for k, v := range s.accounts {
		next[k] = v
	}
	next[a.User] = &a
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.dataFile, data, 0o644); err != nil {
		return err
	}
	s.accounts = next
	return nil
}

// subscribe starts a payment for a plan at Acme Pay.
func (s *Service) subscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
		Plan string `json:"plan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.User == "" {
		writeJSON(w, 400, map[string]string{"error": "expected {\"user\": \"...\", \"plan\": \"basic|pro\"}"})
		return
	}
	amount, ok := plans[req.Plan]
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "unknown plan"})
		return
	}

	body, _ := json.Marshal(map[string]any{"reference": req.User, "amount": amount, "currency": "EUR"})
	resp, err := s.httpClient.Post(s.acmeURL+"/v1/payments", "application/json", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "acme pay unreachable: " + err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var payment struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&payment) != nil {
		writeJSON(w, 502, map[string]string{"error": fmt.Sprintf("acme pay answered %d", resp.StatusCode)})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.get(req.User)
	a.Plan, a.PaymentID, a.Status = req.Plan, payment.ID, "pending"
	if err := s.put(a); err != nil {
		log.Printf("saving account %s: %v", a.User, err)
		writeJSON(w, 503, map[string]string{"error": "storage unavailable"})
		return
	}
	writeJSON(w, 202, map[string]string{"payment_id": payment.ID, "status": payment.Status})
}

func (s *Service) account(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.get(r.PathValue("user"))
	writeJSON(w, 200, map[string]any{"account": a, "access": a.Access()})
}

type event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		PaymentID string `json:"payment_id"`
		Reference string `json:"reference"`
		Amount    int    `json:"amount"`
	} `json:"data"`
}

// webhook applies Acme Pay payment events to the customer's account.
func (s *Service) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "cannot read body"})
		return
	}
	if err := verify(r.Header.Get("Acme-Signature"), body, s.secret, time.Now()); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil || ev.Data.Reference == "" {
		writeJSON(w, 400, map[string]string{"error": "malformed event"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.get(ev.Data.Reference)
	switch ev.Type {
	case "payment.pending":
		a.Status = "pending"
	case "payment.completed":
		a.Status = "active"
		a.Credits += ev.Data.Amount
	case "payment.chargeback":
		a.Status = "revoked"
		a.Credits -= ev.Data.Amount
	default:
		log.Printf("ignoring event %s of type %s", ev.ID, ev.Type)
		writeJSON(w, 200, map[string]bool{"received": true})
		return
	}
	// Acme Pay retries an event that is not answered with 2xx, so a failed
	// save is reported as 503 and the event is applied on the retry.
	if err := s.put(a); err != nil {
		log.Printf("saving account %s for event %s: %v", a.User, ev.ID, err)
		writeJSON(w, 503, map[string]string{"error": "storage unavailable"})
		return
	}
	writeJSON(w, 200, map[string]bool{"received": true})
}

// verify checks an Acme-Signature header of the form t=<unix>,v1=<hex>,
// where v1 is HMAC-SHA256 of "<t>.<body>". Signatures older than five
// minutes are rejected.
func verify(header string, body []byte, secret string, now time.Time) error {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	if ts == "" || sig == "" {
		return errors.New("missing signature")
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("bad signature timestamp")
	}
	if age := now.Sub(time.Unix(unix, 0)); age > 5*time.Minute || age < -5*time.Minute {
		return errors.New("signature timestamp out of tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := env("ADDR", ":3000")
	svc, err := NewService(env("ACME_BASE_URL", "http://localhost:8080"), env("ACME_WEBHOOK_SECRET", "whsec_demo"), env("DATA_FILE", "accounts.json"))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("agent-demo listening on %s, accounts in %s", addr, svc.dataFile)
	log.Fatal(http.ListenAndServe(addr, svc.Handler()))
}
