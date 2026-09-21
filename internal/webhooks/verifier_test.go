package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func githubSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func slackSig(secret, ts, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func genericSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubVerifier(t *testing.T) {
	const secret, body = "whsec", `{"ref":"refs/heads/main"}`
	v := &githubVerifier{}
	err := v.Verify([]byte(body), secret, map[string]string{
		"X-Hub-Signature-256": githubSig(secret, body),
		"X-GitHub-Event":      "push",
	})
	if err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	if got := v.EventType(map[string]string{"X-GitHub-Event": "push"}, nil); got != "push" {
		t.Fatalf("event type = %q", got)
	}
	if err := v.Verify([]byte(body), secret, map[string]string{
		"x-hub-signature-256": githubSig(secret, body),
	}); err != nil {
		t.Fatalf("lowercase header should verify: %v", err)
	}
	if err := v.Verify([]byte(body), secret, map[string]string{
		"X-Hub-Signature-256": githubSig("other", body),
	}); err == nil {
		t.Fatal("expected mismatch")
	}
	if err := v.Verify([]byte(body), secret, nil); err == nil {
		t.Fatal("expected missing header")
	}
}

func TestSlackVerifierRejectsStaleTimestamp(t *testing.T) {
	const secret, body = "slack-secret", `{"type":"event_callback"}`
	fixed := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	slackNow = func() time.Time { return fixed }
	t.Cleanup(func() { slackNow = time.Now })

	v := &slackVerifier{}
	fresh := fmt.Sprintf("%d", fixed.Unix())
	if err := v.Verify([]byte(body), secret, map[string]string{
		"X-Slack-Signature":         slackSig(secret, fresh, body),
		"X-Slack-Request-Timestamp": fresh,
	}); err != nil {
		t.Fatalf("fresh timestamp: %v", err)
	}

	stale := fmt.Sprintf("%d", fixed.Add(-6*time.Minute).Unix())
	if err := v.Verify([]byte(body), secret, map[string]string{
		"X-Slack-Signature":         slackSig(secret, stale, body),
		"X-Slack-Request-Timestamp": stale,
	}); err == nil {
		t.Fatal("expected stale timestamp rejection")
	}

	if err := v.Verify([]byte(body), secret, map[string]string{
		"X-Slack-Signature": slackSig(secret, fresh, body),
	}); err == nil {
		t.Fatal("expected missing timestamp")
	}
}

func TestWhatsAppAndGenericVerifiers(t *testing.T) {
	const secret, body = "meta-secret", `{"entry":[]}`
	wa := &whatsappVerifier{}
	if err := wa.Verify([]byte(body), secret, map[string]string{
		"X-Hub-Signature-256": githubSig(secret, body),
	}); err != nil {
		t.Fatalf("whatsapp: %v", err)
	}

	gen := &genericHMACVerifier{}
	if err := gen.Verify([]byte(body), secret, map[string]string{
		"X-Webhook-Signature": genericSig(secret, body),
	}); err != nil {
		t.Fatalf("generic: %v", err)
	}
	if err := gen.Verify([]byte(body), secret, map[string]string{
		"x-webhook-signature": genericSig(secret, body),
	}); err != nil {
		t.Fatalf("generic lowercase: %v", err)
	}
}

func TestChooseVerifier(t *testing.T) {
	if _, ok := chooseVerifier("com.nomi.github").(*githubVerifier); !ok {
		t.Fatal("github")
	}
	if _, ok := chooseVerifier("com.nomi.slack").(*slackVerifier); !ok {
		t.Fatal("slack")
	}
	if _, ok := chooseVerifier("com.nomi.whatsapp").(*whatsappVerifier); !ok {
		t.Fatal("whatsapp")
	}
	if _, ok := chooseVerifier("com.nomi.teams").(*teamsVerifier); !ok {
		t.Fatal("teams")
	}
	if _, ok := chooseVerifier("com.nomi.email").(*genericHMACVerifier); !ok {
		t.Fatal("generic")
	}
}
