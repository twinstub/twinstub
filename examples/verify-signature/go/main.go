// Minimal webhook receiver that verifies TwinStub signatures.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxSkew = 5 * time.Minute

func verify(secret string, header string, body []byte) error {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		if v, ok := strings.CutPrefix(part, "t="); ok {
			ts = v
		}
		if v, ok := strings.CutPrefix(part, "v1="); ok {
			sig = v
		}
	}
	if ts == "" || sig == "" {
		return fmt.Errorf("malformed signature header %q", header)
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("bad timestamp %q", ts)
	}
	if d := time.Since(time.Unix(unix, 0)); d > maxSkew || d < -maxSkew {
		return fmt.Errorf("timestamp outside the %s window", maxSkew)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func main() {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		secret = "whsec_fintech_demo"
	}
	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", 400)
			return
		}
		if err := verify(secret, r.Header.Get("X-TwinStub-Signature"), body); err != nil {
			log.Printf("signature INVALID (%v): %s", err, body)
			http.Error(w, "bad signature", 401)
			return
		}
		log.Printf("signature OK: %s %s", r.Header.Get("X-TwinStub-Event"), body)
		w.WriteHeader(200)
	})
	log.Println("listening on :9999")
	log.Fatal(http.ListenAndServe(":9999", nil))
}
