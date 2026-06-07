package transport

import (
	"strings"
	"testing"
)

// Direct SMTP / IMAP I/O paths require real network endpoints so they
// live in separate integration tests. Unit coverage here focuses on the
// pure-Go helpers that do header construction and parsing — easy to
// verify without a server.

func TestParseAddress_NormalizesToBareForm(t *testing.T) {
	cases := map[string]string{
		"alice@example.com":         "alice@example.com",
		"Alice <alice@example.com>": "alice@example.com",
		`"Alice B" <alice@ex.com>`:  "alice@ex.com",
	}
	for in, want := range cases {
		got, err := ParseAddress(in)
		if err != nil {
			t.Fatalf("ParseAddress(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractPlainBody_StripsHeaders(t *testing.T) {
	raw := []byte("From: alice@example.com\r\nSubject: hi\r\n\r\nBody text here.")
	got := extractPlainBody(raw)
	if got != "Body text here." {
		t.Fatalf("extractPlainBody did not strip headers: %q", got)
	}
}

func TestExtractPlainBody_HandlesLFOnly(t *testing.T) {
	raw := []byte("From: alice@example.com\nSubject: hi\n\nLF-only body.")
	got := extractPlainBody(raw)
	if got != "LF-only body." {
		t.Fatalf("extractPlainBody LF: %q", got)
	}
}

func TestExtractPlainBody_ReturnsAsIsWhenNoHeaders(t *testing.T) {
	raw := []byte("Just a body.")
	got := extractPlainBody(raw)
	if got != "Just a body." {
		t.Fatalf("extractPlainBody: %q", got)
	}
}

func TestSendEmail_ErrorsOnMissingHost(t *testing.T) {
	err := SendEmail(Config{}, []string{"bob@example.com"}, "hi", "body", "", nil)
	if err == nil {
		t.Fatal("expected error for missing smtp host")
	}
	if !strings.Contains(err.Error(), "smtp host") {
		t.Fatalf("expected 'smtp host' in error, got: %v", err)
	}
}

func TestSendEmail_ErrorsOnMissingFromAndUsername(t *testing.T) {
	err := SendEmail(Config{SMTPHost: "smtp.example.com"}, []string{"bob@example.com"}, "hi", "body", "", nil)
	if err == nil {
		t.Fatal("expected error when both from and username are empty")
	}
}

func TestConfigDefaults_FilledInDuringSend(t *testing.T) {
	// SendEmail fills in port=587 and from=username when not supplied.
	// This test documents the contract; it doesn't send.
	cfg := Config{
		SMTPHost: "smtp.example.invalid", // will fail to dial
		Username: "alice@example.com",
	}
	err := SendEmail(cfg, []string{"bob@example.com"}, "hi", "body", "", nil)
	// We expect a network error here (invalid host), not a config error.
	// The test passes as long as we got past the "from required" guard.
	if err == nil {
		t.Fatal("expected dial error for invalid host")
	}
	if strings.Contains(err.Error(), "is required") {
		t.Fatalf("config-fill-in regressed: %v", err)
	}
}

func TestExtractMessageIDListFromHeader_References(t *testing.T) {
	raw := []byte("From: alice@example.com\r\nReferences: <a@x> <b@y>\r\n\r\nBody")
	ids := extractMessageIDListFromHeader(raw, "References")
	if len(ids) != 2 || ids[0] != "<a@x>" || ids[1] != "<b@y>" {
		t.Fatalf("unexpected references: %#v", ids)
	}
}

func TestExtractMessageIDListFromHeader_Folded(t *testing.T) {
	raw := []byte("From: alice@example.com\r\nReferences: <a@x>\r\n\t<b@y>\r\n\r\nBody")
	ids := extractMessageIDListFromHeader(raw, "References")
	if len(ids) != 2 || ids[0] != "<a@x>" || ids[1] != "<b@y>" {
		t.Fatalf("unexpected folded references: %#v", ids)
	}
}

func TestExtractFirstMessageIDFromHeader_InReplyTo(t *testing.T) {
	raw := []byte("Subject: hi\nIn-Reply-To: <parent@x>\n\nBody")
	id := extractFirstMessageIDFromHeader(raw, "In-Reply-To")
	if id != "<parent@x>" {
		t.Fatalf("got %q, want <parent@x>", id)
	}
}
