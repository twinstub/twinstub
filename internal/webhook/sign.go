package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/twinstub/twinstub/internal/config"
)

// Sign computes the signature header value for a webhook body.
//
// When the format contains {timestamp} the signed string is
// "<unix_seconds>.<raw_body>" (Stripe style). Without it only the raw body
// is signed. The signature is hex, lowercase.
func Sign(s *config.Signing, body []byte, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	withTS := strings.Contains(s.Format, "{timestamp}")

	mac := hmac.New(sha256.New, []byte(s.Secret))
	if withTS {
		mac.Write([]byte(ts))
		mac.Write([]byte("."))
	}
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	out := strings.ReplaceAll(s.Format, "{timestamp}", ts)
	out = strings.ReplaceAll(out, "{signature}", sig)
	return out
}
