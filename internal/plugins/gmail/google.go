package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.klarlabs.de/nomi/internal/integrations/google"
)

// GoogleProvider implements Provider against Gmail REST v1
// (gmail.googleapis.com/gmail/v1). Same shape as the calendar
// GoogleProvider — direct REST calls rather than pulling in
// google.golang.org/api/gmail/v1 to keep the dependency footprint
// small and stable.
type GoogleProvider struct {
	oauth     *google.OAuthManager
	clientID  string
	accountID string
	http      *http.Client
	apiBase   string
}

// NewGoogleProvider constructs a Gmail provider bound to one
// (account, client) pair. The OAuthManager must already hold a
// refresh token for accountID under the manager's standard key
// scheme — that's the state the device-flow endpoints leave behind.
func NewGoogleProvider(oauth *google.OAuthManager, clientID, accountID string) *GoogleProvider {
	return &GoogleProvider{
		oauth:     oauth,
		clientID:  clientID,
		accountID: accountID,
		http:      &http.Client{Timeout: 30 * time.Second},
		apiBase:   "https://gmail.googleapis.com/gmail/v1",
	}
}

// authedRequest fetches a fresh access token via the OAuth manager
// and attaches it as a Bearer header. Centralizes the auth dance so
// the per-tool wrappers stay focused on the API call.
func (g *GoogleProvider) authedRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	tok, err := g.oauth.GetToken(ctx, g.accountID, g.clientID)
	if err != nil {
		return nil, fmt.Errorf("gmail: get access token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	return req, nil
}

// doJSON wraps the request/response cycle: send, decode JSON or
// surface the API's error body. Bounded body read (1 MiB) protects
// the daemon from a runaway response — well above any reasonable
// Gmail message envelope size.
func (g *GoogleProvider) doJSON(req *http.Request, out interface{}) error {
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("gmail: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Check for 401 Unauthorized - invalidate the account's auth
		if resp.StatusCode == http.StatusUnauthorized && g.oauth != nil {
			// Invalidate the account's refresh token so next call triggers re-auth
			_ = g.oauth.InvalidateAccount(g.accountID)
		}
		return fmt.Errorf("gmail: %s %s -> %d: %s", req.Method, req.URL.Path, resp.StatusCode, string(body))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// --- Send ----------------------------------------------------------

// Send writes opts.Body / opts.HTML / opts.Attachments into an RFC
// 5322 message (multipart/alternative when both text + html present;
// multipart/mixed when there are attachments), base64url-encodes the
// whole thing, and POSTs to users/me/messages/send (or .../drafts
// when opts.Draft).
func (g *GoogleProvider) Send(ctx context.Context, opts SendOptions) (SendResult, error) {
	raw, err := buildRFC822(opts)
	if err != nil {
		return SendResult{}, fmt.Errorf("gmail: encode message: %w", err)
	}
	encoded := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(raw)
	payload := map[string]any{"raw": encoded}
	if opts.ThreadID != "" {
		payload["threadId"] = opts.ThreadID
	}

	endpoint := "/users/me/messages/send"
	if opts.Draft {
		endpoint = "/users/me/drafts"
		payload = map[string]any{"message": payload}
	}
	bodyJSON, _ := json.Marshal(payload)
	req, err := g.authedRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyJSON))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	if opts.Draft {
		var resp struct {
			ID      string `json:"id"`
			Message struct {
				ID       string `json:"id"`
				ThreadID string `json:"threadId"`
			} `json:"message"`
		}
		if err := g.doJSON(req, &resp); err != nil {
			return SendResult{}, err
		}
		return SendResult{MessageID: resp.ID, ThreadID: resp.Message.ThreadID, IsDraft: true}, nil
	}
	var resp struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	if err := g.doJSON(req, &resp); err != nil {
		return SendResult{}, err
	}
	return SendResult{MessageID: resp.ID, ThreadID: resp.ThreadID}, nil
}

// --- SearchThreads + ReadThread -----------------------------------

func (g *GoogleProvider) SearchThreads(ctx context.Context, query string, limit int) ([]Thread, error) {
	q := url.Values{}
	q.Set("q", query)
	if limit > 0 {
		q.Set("maxResults", strconv.Itoa(limit))
	}
	req, err := g.authedRequest(ctx, http.MethodGet, "/users/me/threads?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Threads []struct {
			ID      string `json:"id"`
			Snippet string `json:"snippet"`
		} `json:"threads"`
	}
	if err := g.doJSON(req, &resp); err != nil {
		return nil, err
	}
	out := make([]Thread, 0, len(resp.Threads))
	for _, t := range resp.Threads {
		out = append(out, Thread{ID: t.ID, Snippet: t.Snippet})
	}
	return out, nil
}

func (g *GoogleProvider) ReadThread(ctx context.Context, threadID string) (Thread, error) {
	req, err := g.authedRequest(ctx, http.MethodGet, "/users/me/threads/"+url.PathEscape(threadID)+"?format=full", nil)
	if err != nil {
		return Thread{}, err
	}
	var resp gmailThread
	if err := g.doJSON(req, &resp); err != nil {
		return Thread{}, err
	}
	out := Thread{ID: resp.ID, Snippet: resp.Snippet, Messages: make([]Message, 0, len(resp.Messages))}
	for _, m := range resp.Messages {
		msg := decodeGmailMessage(m)
		out.Messages = append(out.Messages, msg)
		if out.Subject == "" {
			out.Subject = msg.Subject
		}
	}
	return out, nil
}

// --- GetMessage ---------------------------------------------------

func (g *GoogleProvider) GetMessage(ctx context.Context, messageID string) (Message, error) {
	req, err := g.authedRequest(ctx, http.MethodGet,
		"/users/me/messages/"+url.PathEscape(messageID)+"?format=full", nil)
	if err != nil {
		return Message{}, err
	}
	var m gmailMessage
	if err := g.doJSON(req, &m); err != nil {
		return Message{}, err
	}
	return decodeGmailMessage(m), nil
}

// --- History API --------------------------------------------------

// LatestHistoryID asks Gmail for the user's current historyId via the
// profile endpoint. Triggers call this on first poll to get the
// baseline they'll use as startHistoryID on the next poll.
func (g *GoogleProvider) LatestHistoryID(ctx context.Context) (string, error) {
	req, err := g.authedRequest(ctx, http.MethodGet, "/users/me/profile", nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		HistoryID string `json:"historyId"`
	}
	if err := g.doJSON(req, &resp); err != nil {
		return "", err
	}
	return resp.HistoryID, nil
}

// History queries users/me/history for events newer than
// startHistoryID. We restrict historyTypes to messageAdded +
// labelAdded — the two deltas v1 triggers care about. The optional
// labelFilter narrows the server-side response (Gmail accepts
// labelId=<id>) but we still run a defensive client-side filter
// because Gmail's history API can return labelAdded events for
// related labels.
func (g *GoogleProvider) History(ctx context.Context, startHistoryID, labelFilter string) ([]HistoryEvent, string, error) {
	if startHistoryID == "" {
		return nil, "", fmt.Errorf("gmail: History requires startHistoryID")
	}
	q := url.Values{}
	q.Set("startHistoryId", startHistoryID)
	q.Add("historyTypes", "messageAdded")
	q.Add("historyTypes", "labelAdded")
	if labelFilter != "" {
		q.Set("labelId", labelFilter)
	}

	req, err := g.authedRequest(ctx, http.MethodGet, "/users/me/history?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		History []struct {
			ID       string `json:"id"`
			Messages []struct {
				ID       string `json:"id"`
				ThreadID string `json:"threadId"`
			} `json:"messages"`
			MessagesAdded []struct {
				Message struct {
					ID       string   `json:"id"`
					ThreadID string   `json:"threadId"`
					LabelIDs []string `json:"labelIds"`
				} `json:"message"`
			} `json:"messagesAdded"`
			LabelsAdded []struct {
				Message struct {
					ID       string `json:"id"`
					ThreadID string `json:"threadId"`
				} `json:"message"`
				LabelIDs []string `json:"labelIds"`
			} `json:"labelsAdded"`
		} `json:"history"`
		HistoryID string `json:"historyId"`
	}
	if err := g.doJSON(req, &resp); err != nil {
		return nil, "", err
	}

	var events []HistoryEvent
	for _, h := range resp.History {
		for _, ma := range h.MessagesAdded {
			events = append(events, HistoryEvent{
				HistoryID:     h.ID,
				Kind:          HistoryMessageAdded,
				MessageID:     ma.Message.ID,
				ThreadID:      ma.Message.ThreadID,
				AddedLabelIDs: ma.Message.LabelIDs,
			})
		}
		for _, la := range h.LabelsAdded {
			events = append(events, HistoryEvent{
				HistoryID:     h.ID,
				Kind:          HistoryLabelAdded,
				MessageID:     la.Message.ID,
				ThreadID:      la.Message.ThreadID,
				AddedLabelIDs: la.LabelIDs,
			})
		}
	}
	// resp.HistoryID is Gmail's mailbox-current id; if the response
	// has no events at all, fall back to startHistoryID so the next
	// poll picks up where this one was supposed to.
	newest := resp.HistoryID
	if newest == "" {
		newest = startHistoryID
	}
	return events, newest, nil
}

// --- Label + Archive ----------------------------------------------

func (g *GoogleProvider) Label(ctx context.Context, messageID string, add, remove []string) error {
	body, _ := json.Marshal(map[string][]string{
		"addLabelIds":    add,
		"removeLabelIds": remove,
	})
	req, err := g.authedRequest(ctx, http.MethodPost,
		"/users/me/messages/"+url.PathEscape(messageID)+"/modify",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return g.doJSON(req, nil)
}

func (g *GoogleProvider) Archive(ctx context.Context, messageID string) error {
	return g.Label(ctx, messageID, nil, []string{"INBOX"})
}

// --- helpers ------------------------------------------------------

// buildRFC822 produces the bytes Gmail's `raw` field accepts. Logic:
//   - If neither HTML nor attachments → plain text/plain message.
//   - If HTML but no attachments → multipart/alternative (text + html).
//   - If attachments → multipart/mixed wrapping the body part(s).
//
// Headers are written manually to avoid the net/mail formatter's
// quirks around address lists and to keep encoding explicit.
func buildRFC822(opts SendOptions) ([]byte, error) {
	if len(opts.To) == 0 {
		return nil, fmt.Errorf("at least one To address required")
	}

	var buf bytes.Buffer
	writeHeader := func(name, value string) {
		fmt.Fprintf(&buf, "%s: %s\r\n", name, value)
	}
	writeHeader("To", strings.Join(opts.To, ", "))
	if len(opts.Cc) > 0 {
		writeHeader("Cc", strings.Join(opts.Cc, ", "))
	}
	if len(opts.Bcc) > 0 {
		writeHeader("Bcc", strings.Join(opts.Bcc, ", "))
	}
	writeHeader("Subject", opts.Subject)
	writeHeader("MIME-Version", "1.0")

	hasHTML := opts.HTML != ""
	hasAttachments := len(opts.Attachments) > 0

	switch {
	case !hasHTML && !hasAttachments:
		writeHeader("Content-Type", `text/plain; charset="utf-8"`)
		writeHeader("Content-Transfer-Encoding", "quoted-printable")
		buf.WriteString("\r\n")
		qp := quotedprintable.NewWriter(&buf)
		_, _ = qp.Write([]byte(opts.Body))
		_ = qp.Close()
	case !hasAttachments:
		// multipart/alternative — text + html
		mw := multipart.NewWriter(&buf)
		writeHeader("Content-Type", `multipart/alternative; boundary=`+strconv.Quote(mw.Boundary()))
		buf.WriteString("\r\n")
		if err := writeAltPart(mw, "text/plain", opts.Body); err != nil {
			return nil, err
		}
		if err := writeAltPart(mw, "text/html", opts.HTML); err != nil {
			return nil, err
		}
		_ = mw.Close()
	default:
		// multipart/mixed — body (possibly multipart/alternative) + attachments
		mw := multipart.NewWriter(&buf)
		writeHeader("Content-Type", `multipart/mixed; boundary=`+strconv.Quote(mw.Boundary()))
		buf.WriteString("\r\n")
		// Body part: nested multipart/alternative if HTML is present,
		// otherwise a single text/plain part.
		if hasHTML {
			if err := writeNestedAlternative(mw, opts.Body, opts.HTML); err != nil {
				return nil, err
			}
		} else {
			if err := writeAltPart(mw, "text/plain", opts.Body); err != nil {
				return nil, err
			}
		}
		for _, att := range opts.Attachments {
			if err := writeAttachmentPart(mw, att); err != nil {
				return nil, err
			}
		}
		_ = mw.Close()
	}
	return buf.Bytes(), nil
}

func writeAltPart(mw *multipart.Writer, contentType, body string) error {
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Type", contentType+`; charset="utf-8"`)
	hdr.Set("Content-Transfer-Encoding", "quoted-printable")
	w, err := mw.CreatePart(hdr)
	if err != nil {
		return err
	}
	qp := quotedprintable.NewWriter(w)
	_, err = qp.Write([]byte(body))
	if err != nil {
		return err
	}
	return qp.Close()
}

// writeNestedAlternative emits a multipart/alternative part inside
// `parent`. The two writers must agree on the boundary string so the
// Content-Type header matches the part-separator bytes the inner
// writer produces — picking the boundary up front via a discarded
// writer and then re-pinning it on the real destination is the
// stdlib idiom for that.
func writeNestedAlternative(parent *multipart.Writer, text, html string) error {
	hdr := textproto.MIMEHeader{}
	boundary := multipart.NewWriter(io.Discard).Boundary()
	hdr.Set("Content-Type", `multipart/alternative; boundary=`+strconv.Quote(boundary))
	w, err := parent.CreatePart(hdr)
	if err != nil {
		return err
	}
	inner := multipart.NewWriter(w)
	if err := inner.SetBoundary(boundary); err != nil {
		return err
	}
	if err := writeAltPart(inner, "text/plain", text); err != nil {
		return err
	}
	if err := writeAltPart(inner, "text/html", html); err != nil {
		return err
	}
	return inner.Close()
}

func writeAttachmentPart(mw *multipart.Writer, att Attachment) error {
	hdr := textproto.MIMEHeader{}
	ct := att.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	hdr.Set("Content-Type", ct+`; name=`+strconv.Quote(att.Filename))
	disposition := "attachment"
	if att.ContentID != "" {
		disposition = "inline"
		hdr.Set("Content-ID", "<"+att.ContentID+">")
	}
	hdr.Set("Content-Disposition", disposition+`; filename=`+strconv.Quote(att.Filename))
	hdr.Set("Content-Transfer-Encoding", "base64")
	w, err := mw.CreatePart(hdr)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(att.Data)
	// Wrap the base64 stream at 76 chars per RFC 2045.
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		if _, err := w.Write([]byte(encoded[i:end] + "\r\n")); err != nil {
			return err
		}
	}
	return nil
}

// --- response decoding --------------------------------------------

type gmailThread struct {
	ID       string         `json:"id"`
	Snippet  string         `json:"snippet"`
	Messages []gmailMessage `json:"messages"`
}

type gmailMessage struct {
	ID           string    `json:"id"`
	ThreadID     string    `json:"threadId"`
	LabelIDs     []string  `json:"labelIds"`
	Snippet      string    `json:"snippet"`
	InternalDate string    `json:"internalDate"`
	Payload      gmailPart `json:"payload"`
}

type gmailPart struct {
	MimeType string        `json:"mimeType"`
	Headers  []gmailHeader `json:"headers"`
	Body     gmailPartBody `json:"body"`
	Parts    []gmailPart   `json:"parts"`
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailPartBody struct {
	Data string `json:"data"`
	Size int    `json:"size"`
}

func decodeGmailMessage(m gmailMessage) Message {
	out := Message{
		ID:       m.ID,
		ThreadID: m.ThreadID,
		Labels:   m.LabelIDs,
		Snippet:  m.Snippet,
	}
	for _, h := range m.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			out.From = h.Value
		case "to":
			out.To = splitAddressList(h.Value)
		case "cc":
			out.Cc = splitAddressList(h.Value)
		case "subject":
			out.Subject = h.Value
		}
	}
	if ms, err := strconv.ParseInt(m.InternalDate, 10, 64); err == nil {
		out.Date = time.UnixMilli(ms).UTC()
	}
	out.BodyText, out.BodyHTML = extractBodies(m.Payload)
	return out
}

// extractBodies walks the MIME tree and pulls the first text/plain
// and text/html bodies it finds. Multipart messages typically have
// both; we surface both so the caller can pick which to show.
func extractBodies(p gmailPart) (text, html string) {
	if strings.HasPrefix(p.MimeType, "text/plain") && p.Body.Data != "" {
		text = decodeURLBase64(p.Body.Data)
	}
	if strings.HasPrefix(p.MimeType, "text/html") && p.Body.Data != "" {
		html = decodeURLBase64(p.Body.Data)
	}
	for _, child := range p.Parts {
		if text == "" {
			if t, _ := extractBodies(child); t != "" {
				text = t
			}
		}
		if html == "" {
			if _, h := extractBodies(child); h != "" {
				html = h
			}
		}
	}
	return text, html
}

// decodeURLBase64 decodes Gmail's URL-safe base64 (no padding) body
// data. Returns empty string on decode failure rather than erroring —
// a single malformed attachment shouldn't fail the whole thread read.
func decodeURLBase64(s string) string {
	body, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(s)
	if err != nil {
		return ""
	}
	return string(body)
}

// splitAddressList splits a Gmail header value into individual
// addresses on commas. Doesn't try to parse name<addr> shapes —
// callers that need to display friendly names can do their own
// parsing on the raw string.
func splitAddressList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
