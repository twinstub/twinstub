package webhook

import (
	"testing"
	"time"

	"github.com/twinstub/twinstub/internal/config"
)

// Reference vectors computed with an independent implementation
// (python hmac/hashlib). They pin the wire format: with {timestamp} in the
// format the signed string is "<unix>.<body>", otherwise the body alone.
func TestSignReferenceVectors(t *testing.T) {
	body := `{"event":"payment.succeeded","amount":1999}`
	at := time.Unix(1700000000, 0)

	cases := []struct {
		name   string
		secret string
		format string
		body   string
		want   string
	}{
		{
			name:   "stripe style with timestamp",
			secret: "whsec_test_secret",
			format: "t={timestamp},v1={signature}",
			body:   body,
			want:   "t=1700000000,v1=82510c8c68a253ffb1088b479c6210da740c13cf58a5be61da0b6e06613ed4d3",
		},
		{
			name:   "plain signature, body only",
			secret: "whsec_test_secret",
			format: "{signature}",
			body:   body,
			want:   "64d70551e7474d9b1352c2b4555ef2bf2dcab5e23bb5ec4d471fda5faef65234",
		},
		{
			name:   "different secret",
			secret: "another_secret",
			format: "t={timestamp},v1={signature}",
			body:   body,
			want:   "t=1700000000,v1=d70cfd351a1a67cfc4f5f45d1e2c88e61e0e9246a54d2fae53f709297870df1c",
		},
		{
			name:   "empty body with timestamp",
			secret: "whsec_test_secret",
			format: "t={timestamp},v1={signature}",
			body:   "",
			want:   "t=1700000000,v1=316b9ab98c15bfa039d243f3196acee619cf29749a91a634e6a8154e2f7b6727",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &config.Signing{Algorithm: "hmac-sha256", Secret: c.secret, Format: c.format}
			got := Sign(s, []byte(c.body), at)
			if got != c.want {
				t.Errorf("Sign = %q\nwant %q", got, c.want)
			}
		})
	}
}
