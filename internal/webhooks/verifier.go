package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Verifier checks whether a webhook payload was sent by the claimed
// provider using a shared secret.
type Verifier interface {
	// Verify returns nil if the signature is valid.
	Verify(body []byte, secret string, headers map[string]string) error
	// EventType extracts the event type from headers or body.
	EventType(headers map[string]string, body []byte) string
}

// chooseVerifier picks the right verifier based on plugin ID.
func chooseVerifier(pluginID string) Verifier {
	switch {
	case strings.Contains(pluginID, "github"):
		return &githubVerifier{}
	case strings.Contains(pluginID, "slack"):
		return &slackVerifier{}
	case strings.Contains(pluginID, "whatsapp"):
		return &whatsappVerifier{}
	default:
		return &genericHMACVerifier{}
	}
}

// ---------------------------------------------------------------------------
// GitHub verifier — X-Hub-Signature-256 (HMAC-SHA256 hex)
// ---------------------------------------------------------------------------

type githubVerifier struct{}

func (v *githubVerifier) Verify(body []byte, secret string, headers map[string]string) error {
	sig := headers["X-Hub-Signature-256"]
	if sig == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}
	// GitHub prefixes with "sha256="
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return fmt.Errorf("invalid signature format")
	}
	expectedHex := strings.TrimPrefix(sig, prefix)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected, err := hex.DecodeString(expectedHex)
	if err != nil {
		return fmt.Errorf("invalid signature hex: %w", err)
	}
	if !hmac.Equal(mac.Sum(nil), expected) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func (v *githubVerifier) EventType(headers map[string]string, body []byte) string {
	return headers["X-GitHub-Event"]
}

// ---------------------------------------------------------------------------
// Slack verifier — v0 signing scheme (HMAC-SHA256 base64)
// ---------------------------------------------------------------------------

type slackVerifier struct{}

func (v *slackVerifier) Verify(body []byte, secret string, headers map[string]string) error {
	sig := headers["X-Slack-Signature"]
	if sig == "" {
		return fmt.Errorf("missing X-Slack-Signature header")
	}
	ts := headers["X-Slack-Request-Timestamp"]
	if ts == "" {
		return fmt.Errorf("missing X-Slack-Request-Timestamp header")
	}

	// Slack signature base string: "v0:timestamp:body"
	base := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(base))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func (v *slackVerifier) EventType(headers map[string]string, body []byte) string {
	// Slack event type is in the JSON body under "type" or "event.type"
	// For simplicity, return the body type field.
	return "slack_event"
}

// ---------------------------------------------------------------------------
// WhatsApp verifier — X-Hub-Signature-256 (HMAC-SHA256 hex with sha256= prefix),
// signed with the Meta App Secret. Same signature scheme as GitHub; the
// event-type extraction differs and the secret comes from a different
// connection key, so it gets its own verifier rather than aliasing.
// ---------------------------------------------------------------------------

type whatsappVerifier struct{}

func (v *whatsappVerifier) Verify(body []byte, secret string, headers map[string]string) error {
	sig := headers["X-Hub-Signature-256"]
	if sig == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return fmt.Errorf("invalid signature format")
	}
	expectedHex := strings.TrimPrefix(sig, prefix)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected, err := hex.DecodeString(expectedHex)
	if err != nil {
		return fmt.Errorf("invalid signature hex: %w", err)
	}
	if !hmac.Equal(mac.Sum(nil), expected) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func (v *whatsappVerifier) EventType(headers map[string]string, body []byte) string {
	// WhatsApp Cloud API webhooks carry the event kind under
	// entry[].changes[].field. "messages" is the only field we care
	// about today; future code can split out "statuses" etc.
	return "whatsapp_event"
}

// ---------------------------------------------------------------------------
// Generic HMAC verifier — X-Webhook-Signature (HMAC-SHA256 hex)
// ---------------------------------------------------------------------------

type genericHMACVerifier struct{}

func (v *genericHMACVerifier) Verify(body []byte, secret string, headers map[string]string) error {
	sig := headers["X-Webhook-Signature"]
	if sig == "" {
		return fmt.Errorf("missing X-Webhook-Signature header")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func (v *genericHMACVerifier) EventType(headers map[string]string, body []byte) string {
	return headers["X-Webhook-Event"]
}
