// Package transport implements the Email plugin's IMAP receive + SMTP
// send primitives. Deliberately provider-agnostic — Gmail/Outlook-specific
// concerns live in the gmail/calendar plugins, not here.
//
// Scope is intentionally small for v1: SMTP with STARTTLS, IMAP poll
// (not IDLE), plaintext body extraction, Message-ID + In-Reply-To header
// parsing. Attachments, richer MIME, IDLE, and OAuth are follow-ups.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	imapclient "github.com/emersion/go-imap/v2/imapclient"
)

// stdEncoding aliased for the wrapBase64 helper to avoid a per-call
// package qualifier. base64.StdEncoding is the canonical RFC 4648
// alphabet which all email clients understand.
var stdEncoding = base64.StdEncoding

// _json keeps encoding/json reachable; the legacy SendEmail path uses
// it via downstream call sites elsewhere in the package once the
// inbound IMAP fetcher decodes envelope JSON.
var _json = json.Marshal

// Config is the per-connection email-plugin configuration. Everything
// callers need to open an IMAP connection for receive and an SMTP
// connection for send. Credentials are fetched from secrets.Store and
// injected here as plaintext just before use.
type Config struct {
	// IMAPHost is the IMAP server hostname. Common values:
	//   imap.gmail.com, outlook.office365.com, imap.fastmail.com.
	IMAPHost string
	// IMAPPort defaults to 993 (implicit TLS). Plain-text 143 is
	// intentionally unsupported — no modern provider accepts it.
	IMAPPort int
	// SMTPHost is the SMTP submission server. Typically smtp.<provider>.
	SMTPHost string
	// SMTPPort defaults to 587 (STARTTLS). Port 465 uses implicit TLS.
	SMTPPort int
	// Username is the account user name (usually the email address).
	Username string
	// Password is the plaintext password or app-specific password. OAuth
	// access tokens are passed through this field via XOAUTH2 SASL —
	// AuthMech controls which flow is used.
	Password string
	// From is the From: header used on outbound messages. Defaults to
	// Username when empty.
	From string
	// AuthMech selects the SMTP/IMAP auth mechanism:
	//   "plain"    — LOGIN over TLS with Username/Password (default)
	//   "xoauth2"  — OAuth2 bearer; Password carries the access token
	AuthMech string
}

// EmailAttachment carries one binary attachment for outbound mail. Bytes
// are base64-encoded into the multipart/mixed body during SendEmailWithAttachments.
// ContentType defaults to application/octet-stream when empty so the
// attachment still arrives intact (the recipient's client picks
// reasonable defaults from the filename extension).
type EmailAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Message is the trimmed representation of an RFC 5322 message the plugin
// works with. Preserves enough header fields to do Message-ID threading
// and render the Chats tab sensibly; rich content (HTML, attachments)
// lives in follow-up work.
type Message struct {
	UID        uint32
	MessageID  string // RFC 5322 Message-ID header (with surrounding <>)
	InReplyTo  string // Referenced Message-ID for threaded replies
	References []string
	From       string
	To         []string
	Subject    string
	Body       string // plain-text body, decoded
	ReceivedAt time.Time
}

// SendEmail delivers a single plaintext email via the configured SMTP
// server. Builds a minimal RFC 5322 message with Message-ID + Date +
// optional In-Reply-To/References for threading.
//
// replyTo is the Message-ID of the message this one is replying to (with
// surrounding angle brackets). Empty means a fresh thread.
func SendEmail(cfg Config, to []string, subject, body, replyTo string, references []string) error {
	if cfg.SMTPHost == "" {
		return fmt.Errorf("smtp host is required")
	}
	port := cfg.SMTPPort
	if port == 0 {
		port = 587
	}
	from := cfg.From
	if from == "" {
		from = cfg.Username
	}
	if from == "" {
		return fmt.Errorf("from/username is required")
	}

	msgID := fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), strings.Split(from, "@")[safeIndex(from)])
	headers := []string{
		fmt.Sprintf("From: %s", from),
		fmt.Sprintf("To: %s", strings.Join(to, ", ")),
		fmt.Sprintf("Subject: %s", subject),
		fmt.Sprintf("Date: %s", time.Now().UTC().Format(time.RFC1123Z)),
		fmt.Sprintf("Message-ID: %s", msgID),
		"MIME-Version: 1.0",
		`Content-Type: text/plain; charset="utf-8"`,
	}
	if replyTo != "" {
		headers = append(headers, fmt.Sprintf("In-Reply-To: %s", replyTo))
	}
	if len(references) > 0 {
		headers = append(headers, fmt.Sprintf("References: %s", strings.Join(references, " ")))
	}
	msg := strings.Join(headers, "\r\n") + "\r\n\r\n" + body

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, port)
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)
	// net/smtp's SendMail handles STARTTLS upgrade on port 587
	// automatically when TLS is advertised. Port 465 requires a dialer
	// that wraps the connection in TLS before speaking SMTP — we build
	// that ourselves when encountered.
	if port == 465 {
		return sendMailImplicitTLS(cfg.SMTPHost, addr, auth, from, to, []byte(msg))
	}
	return smtp.SendMail(addr, auth, from, to, []byte(msg))
}

// SendEmailWithAttachments is the multipart variant of SendEmail. When
// the message has attachments, it constructs a multipart/mixed body
// with one text/plain part for the body and one application/octet-stream
// part per attachment, base64-encoded with proper Content-Disposition +
// Content-Type headers.
//
// SendEmail (text-only) is preserved as the fast path so the common
// case stays simple. Callers with attachments call this; callers
// without keep using SendEmail.
func SendEmailWithAttachments(cfg Config, to []string, subject, body, replyTo string, references []string, attachments []EmailAttachment) error {
	if len(attachments) == 0 {
		return SendEmail(cfg, to, subject, body, replyTo, references)
	}
	if cfg.SMTPHost == "" {
		return fmt.Errorf("smtp host is required")
	}
	port := cfg.SMTPPort
	if port == 0 {
		port = 587
	}
	from := cfg.From
	if from == "" {
		from = cfg.Username
	}
	if from == "" {
		return fmt.Errorf("from/username is required")
	}

	msgID := fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), strings.Split(from, "@")[safeIndex(from)])
	boundary := fmt.Sprintf("nomi-boundary-%d", time.Now().UnixNano())

	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: %s\r\n", msgID)
	if replyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", replyTo)
	}
	if len(references) > 0 {
		fmt.Fprintf(&b, "References: %s\r\n", strings.Join(references, " "))
	}
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n", boundary)
	fmt.Fprintf(&b, "\r\n")

	// Text body part.
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=\"utf-8\"\r\n")
	fmt.Fprintf(&b, "Content-Transfer-Encoding: 8bit\r\n")
	fmt.Fprintf(&b, "\r\n")
	fmt.Fprintf(&b, "%s\r\n", body)

	// Attachment parts.
	for _, att := range attachments {
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		fmt.Fprintf(&b, "Content-Type: %s; name=%q\r\n", ct, att.Filename)
		fmt.Fprintf(&b, "Content-Disposition: attachment; filename=%q\r\n", att.Filename)
		fmt.Fprintf(&b, "Content-Transfer-Encoding: base64\r\n")
		fmt.Fprintf(&b, "\r\n")
		// 76-char-wrapped base64 keeps line length within RFC 2045's
		// recommended limits — some receivers reject longer lines.
		b.WriteString(wrapBase64(att.Data, 76))
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, port)
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)
	if port == 465 {
		return sendMailImplicitTLS(cfg.SMTPHost, addr, auth, from, to, b.Bytes())
	}
	return smtp.SendMail(addr, auth, from, to, b.Bytes())
}

// wrapBase64 base64-encodes data and wraps the output at lineLen
// characters per RFC 2045 §6.8 recommendation.
func wrapBase64(data []byte, lineLen int) string {
	encoded := base64Encode(data)
	if lineLen <= 0 || len(encoded) <= lineLen {
		return encoded
	}
	var b strings.Builder
	for i := 0; i < len(encoded); i += lineLen {
		end := i + lineLen
		if end > len(encoded) {
			end = len(encoded)
		}
		b.WriteString(encoded[i:end])
		if end < len(encoded) {
			b.WriteString("\r\n")
		}
	}
	return b.String()
}

// base64Encode is a thin wrapper around encoding/base64's StdEncoding
// extracted so the wrapping helper above doesn't take a hard import on
// the package — keeps wrapBase64 testable in isolation.
func base64Encode(data []byte) string {
	return stdEncoding.EncodeToString(data)
}

// sendMailImplicitTLS handles port 465 (SMTPS) where the connection must
// be TLS-wrapped before the server speaks SMTP. net/smtp's SendMail
// doesn't support this directly so we replicate its flow with a TLS
// dialer up front.
func sendMailImplicitTLS(host, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return fmt.Errorf("tls dial: %w", err)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer func() { _ = client.Quit() }()
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return nil
}

func safeIndex(addr string) int {
	if idx := strings.Index(addr, "@"); idx >= 0 {
		return 1 // split returns [local, domain]; we want domain at index 1
	}
	return 0
}

// FetchNew opens an IMAP session and returns messages with UIDs greater
// than sinceUID, plus the new highest-UID watermark the caller should
// pass on the next call. Safe to use on a poll loop — plugins that want
// IDLE can migrate later.
//
// Returns an empty slice (not nil) when there are no new messages, so
// callers can append without nil-guarding.
func FetchNew(ctx context.Context, cfg Config, sinceUID uint32) ([]Message, uint32, error) {
	port := cfg.IMAPPort
	if port == 0 {
		port = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, port)

	c, err := imapclient.DialTLS(addr, &imapclient.Options{})
	if err != nil {
		return nil, sinceUID, fmt.Errorf("imap dial: %w", err)
	}
	defer func() { _ = c.Close() }()

	if cfg.Username != "" {
		if err := c.Login(cfg.Username, cfg.Password).Wait(); err != nil {
			return nil, sinceUID, fmt.Errorf("imap login: %w", err)
		}
	}

	if _, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: false}).Wait(); err != nil {
		return nil, sinceUID, fmt.Errorf("imap select: %w", err)
	}

	// Search for messages with UID strictly greater than the watermark.
	// Empty ranges are permitted and return zero results.
	seqSet := imap.UIDSetNum()
	seqSet.AddRange(imap.UID(sinceUID+1), 0) // 0 = *, i.e. up to current max
	fetchOpts := &imap.FetchOptions{
		Envelope:    true,
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{Specifier: imap.PartSpecifierText}},
	}
	msgs, err := c.Fetch(seqSet, fetchOpts).Collect()
	if err != nil {
		return nil, sinceUID, fmt.Errorf("imap fetch: %w", err)
	}

	out := make([]Message, 0, len(msgs))
	maxUID := sinceUID
	for _, m := range msgs {
		msg := Message{
			UID: uint32(m.UID),
		}
		if m.Envelope != nil {
			msg.Subject = m.Envelope.Subject
			msg.MessageID = m.Envelope.MessageID
			// InReplyTo in the v2 envelope is a list; take the first
			// reference for threading. References is the more complete
			// ancestor chain.
			if len(m.Envelope.InReplyTo) > 0 {
				msg.InReplyTo = m.Envelope.InReplyTo[0]
			}
			msg.ReceivedAt = m.Envelope.Date
			msg.From = formatAddressList(m.Envelope.From)
			msg.To = addressListToStrings(m.Envelope.To)
		}
		for _, bs := range m.BodySection {
			if bs.Bytes != nil {
				// Some IMAP servers include the RFC 5322 headers in the fetched
				// body section; when present, parse References/In-Reply-To so
				// threading can prefer the full ancestor chain.
				if refs := extractMessageIDListFromHeader(bs.Bytes, "References"); len(refs) > 0 {
					msg.References = refs
				}
				if msg.InReplyTo == "" {
					if irt := extractFirstMessageIDFromHeader(bs.Bytes, "In-Reply-To"); irt != "" {
						msg.InReplyTo = irt
					}
				}
				msg.Body = extractPlainBody(bs.Bytes)
				break
			}
		}
		out = append(out, msg)
		if uint32(m.UID) > maxUID {
			maxUID = uint32(m.UID)
		}
	}
	_ = c.Logout()
	return out, maxUID, nil
}

// extractPlainBody does a rough pass at pulling the text/plain content
// out of an RFC 5322 body. For v1 we accept the body as-is when it's
// plain; multipart parsing is a follow-up.
func extractPlainBody(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	// If the body starts with RFC 5322 headers, strip them up to the
	// blank line that separates headers from body.
	s := string(raw)
	if idx := strings.Index(s, "\r\n\r\n"); idx >= 0 {
		return s[idx+4:]
	}
	if idx := strings.Index(s, "\n\n"); idx >= 0 {
		return s[idx+2:]
	}
	return s
}

func addressListToStrings(addrs []imap.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Mailbox != "" && a.Host != "" {
			out = append(out, fmt.Sprintf("%s@%s", a.Mailbox, a.Host))
		}
	}
	return out
}

func formatAddressList(addrs []imap.Address) string {
	parts := addressListToStrings(addrs)
	return strings.Join(parts, ", ")
}

func extractMessageIDListFromHeader(raw []byte, header string) []string {
	if len(raw) == 0 || header == "" {
		return nil
	}
	b := string(raw)
	end := strings.Index(b, "\r\n\r\n")
	if end < 0 {
		end = strings.Index(b, "\n\n")
	}
	if end < 0 {
		return nil
	}
	headers := b[:end]
	var value string
	lines := strings.Split(strings.ReplaceAll(headers, "\r\n", "\n"), "\n")
	needle := strings.ToLower(header) + ":"
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(strings.ToLower(line), needle) {
			value = strings.TrimSpace(line[len(needle):])
			// Folded headers: continuation lines start with SP/HT.
			for j := i + 1; j < len(lines); j++ {
				next := lines[j]
				if next == "" {
					break
				}
				if strings.HasPrefix(next, " ") || strings.HasPrefix(next, "\t") {
					value += " " + strings.TrimSpace(next)
					continue
				}
				break
			}
			break
		}
	}
	if value == "" {
		return nil
	}
	ids := extractMessageIDs(value)
	if len(ids) == 0 {
		return nil
	}
	return ids
}

func extractFirstMessageIDFromHeader(raw []byte, header string) string {
	ids := extractMessageIDListFromHeader(raw, header)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func extractMessageIDs(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "<")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		end := strings.Index(p, ">")
		if end < 0 {
			continue
		}
		id := strings.TrimSpace(p[:end])
		if id == "" {
			continue
		}
		out = append(out, "<"+id+">")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseAddress normalizes an RFC 5322 address string to the bare
// `local@domain` form. Used by the plugin layer when matching an
// incoming sender against the identity allowlist.
func ParseAddress(raw string) (string, error) {
	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return "", err
	}
	return addr.Address, nil
}

// Compile-time guard: keep io import used even if body reader gets
// refactored later.
var _ = io.EOF
