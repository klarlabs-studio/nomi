package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// stubProvider records calls so each tool's input → Provider-call
// translation can be asserted without a real Gmail account.
type stubProvider struct {
	sentOpts    SendOptions
	sentResult  SendResult
	sentErr     error
	searched    string
	searchLimit int
	searchOut   []Thread
	readID      string
	readOut     Thread
	readErr     error
	labeledID   string
	labelAdd    []string
	labelRemove []string
	labelErr    error
	archivedID  string
	archiveErr  error

	// trigger-related state
	mu              sync.Mutex
	getMessageOut   map[string]Message
	historyEvents   []HistoryEvent
	historyNewestID string
	historyStartIDs []string
	historyErr      error
	latestHistoryID string
	latestErr       error
}

func (s *stubProvider) Send(_ context.Context, opts SendOptions) (SendResult, error) {
	s.sentOpts = opts
	if s.sentErr != nil {
		return SendResult{}, s.sentErr
	}
	if s.sentResult.MessageID == "" {
		s.sentResult.MessageID = "msg-from-stub"
	}
	return s.sentResult, nil
}

func (s *stubProvider) SearchThreads(_ context.Context, q string, limit int) ([]Thread, error) {
	s.searched = q
	s.searchLimit = limit
	return s.searchOut, nil
}

func (s *stubProvider) ReadThread(_ context.Context, id string) (Thread, error) {
	s.readID = id
	if s.readErr != nil {
		return Thread{}, s.readErr
	}
	return s.readOut, nil
}

func (s *stubProvider) Label(_ context.Context, id string, add, remove []string) error {
	s.labeledID = id
	s.labelAdd = add
	s.labelRemove = remove
	return s.labelErr
}

func (s *stubProvider) Archive(_ context.Context, id string) error {
	s.archivedID = id
	return s.archiveErr
}

func (s *stubProvider) GetMessage(_ context.Context, id string) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.getMessageOut[id]; ok {
		return m, nil
	}
	return Message{ID: id}, nil
}

func (s *stubProvider) LatestHistoryID(_ context.Context) (string, error) {
	if s.latestErr != nil {
		return "", s.latestErr
	}
	if s.latestHistoryID == "" {
		return "100", nil
	}
	return s.latestHistoryID, nil
}

func (s *stubProvider) History(_ context.Context, startID, _ string) ([]HistoryEvent, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyStartIDs = append(s.historyStartIDs, startID)
	if s.historyErr != nil {
		return nil, startID, s.historyErr
	}
	newest := s.historyNewestID
	if newest == "" {
		newest = startID
	}
	return s.historyEvents, newest, nil
}

// fixture spins up an in-memory plugin + connection row + stub
// Provider override so tools dispatch end-to-end against the
// recorded stub.
type fixture struct {
	plugin       *Plugin
	connectionID string
	provider     *stubProvider
	connsRepo    *db.ConnectionRepository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	database, err := db.New(db.Config{Path: filepath.Join(tmp, "test.db")})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conns := db.NewConnectionRepository(database)
	binds := db.NewAssistantBindingRepository(database)

	plug := NewPlugin(conns, binds, nil, nil)
	provider := &stubProvider{}
	plug.SetProviderOverride(func(*domain.Connection) (Provider, error) { return provider, nil })

	connID := "conn-test-1"
	if err := conns.Create(&domain.Connection{
		ID:       connID,
		PluginID: PluginID,
		Name:     "test",
		Config:   map[string]any{"provider": "google", "account_id": "acct", "client_id": "cid"},
		Enabled:  true,
	}); err != nil {
		t.Fatalf("conn create: %v", err)
	}
	return &fixture{plugin: plug, connectionID: connID, provider: provider, connsRepo: conns}
}

func (f *fixture) toolByName(name string) func(input map[string]interface{}) (map[string]interface{}, error) {
	for _, tl := range f.plugin.Tools() {
		if tl.Name() == name {
			t := tl
			return func(input map[string]interface{}) (map[string]interface{}, error) {
				return t.Execute(context.Background(), input)
			}
		}
	}
	return nil
}

// --- manifest --------------------------------------------------

func TestManifest_DeclaresAllTools(t *testing.T) {
	plug := NewPlugin(nil, nil, nil, nil)
	m := plug.Manifest()
	if m.ID != PluginID {
		t.Fatalf("manifest id = %q", m.ID)
	}
	wantTools := map[string]bool{
		"gmail.send":           false,
		"gmail.search_threads": false,
		"gmail.read_thread":    false,
		"gmail.label":          false,
		"gmail.archive":        false,
	}
	for _, tc := range m.Contributes.Tools {
		if _, ok := wantTools[tc.Name]; ok {
			wantTools[tc.Name] = true
		}
	}
	for name, seen := range wantTools {
		if !seen {
			t.Errorf("tool %q missing from manifest", name)
		}
	}
}

func TestManifest_NetworkAllowlistIncludesGmailHost(t *testing.T) {
	m := NewPlugin(nil, nil, nil, nil).Manifest()
	hosts := m.Requires.NetworkAllowlist
	want := "gmail.googleapis.com"
	for _, h := range hosts {
		if h == want {
			return
		}
	}
	t.Fatalf("manifest allowlist missing %q (got %v)", want, hosts)
}

// --- tool dispatch --------------------------------------------

func TestSend_RoutesInputToProvider(t *testing.T) {
	f := newFixture(t)
	send := f.toolByName("gmail.send")
	if send == nil {
		t.Fatal("gmail.send tool not found")
	}
	out, err := send(map[string]interface{}{
		"connection_id": f.connectionID,
		"to":            []interface{}{"alice@example.com", "bob@example.com"},
		"subject":       "hello",
		"body":          "first message",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if out["message_id"] == "" {
		t.Fatalf("expected message_id in result: %+v", out)
	}
	if got := f.provider.sentOpts.To; len(got) != 2 || got[0] != "alice@example.com" {
		t.Fatalf("To not threaded through: %v", got)
	}
	if f.provider.sentOpts.Subject != "hello" {
		t.Fatalf("Subject not threaded through")
	}
}

func TestSend_RejectsMissingTo(t *testing.T) {
	f := newFixture(t)
	send := f.toolByName("gmail.send")
	_, err := send(map[string]interface{}{
		"connection_id": f.connectionID,
		"subject":       "x",
		"body":          "y",
	})
	if err == nil {
		t.Fatal("expected error for missing To")
	}
	if !strings.Contains(err.Error(), "to is required") {
		t.Fatalf("error should mention To, got %v", err)
	}
}

func TestSend_DecodesBase64Attachment(t *testing.T) {
	f := newFixture(t)
	send := f.toolByName("gmail.send")
	payload := []byte("hello attachment")
	encoded := base64.StdEncoding.EncodeToString(payload)
	_, err := send(map[string]interface{}{
		"connection_id": f.connectionID,
		"to":            "alice@example.com",
		"subject":       "with file",
		"body":          "see attached",
		"attachments": []interface{}{
			map[string]interface{}{
				"filename":     "note.txt",
				"content_type": "text/plain",
				"data_base64":  encoded,
			},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(f.provider.sentOpts.Attachments) != 1 {
		t.Fatalf("attachment not passed through")
	}
	if string(f.provider.sentOpts.Attachments[0].Data) != string(payload) {
		t.Fatalf("attachment bytes mismatch")
	}
}

func TestSend_RejectsBadBase64Attachment(t *testing.T) {
	f := newFixture(t)
	send := f.toolByName("gmail.send")
	_, err := send(map[string]interface{}{
		"connection_id": f.connectionID,
		"to":            "alice@example.com",
		"attachments": []interface{}{
			map[string]interface{}{"filename": "x", "data_base64": "not-base64!!"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "data_base64 invalid") {
		t.Fatalf("expected base64 decode error, got %v", err)
	}
}

func TestSearchThreads_PassesQueryAndLimit(t *testing.T) {
	f := newFixture(t)
	f.provider.searchOut = []Thread{{ID: "t1", Snippet: "first"}, {ID: "t2", Snippet: "second"}}
	search := f.toolByName("gmail.search_threads")
	out, err := search(map[string]interface{}{
		"connection_id": f.connectionID,
		"query":         "from:alice has:attachment",
		"limit":         float64(50),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if f.provider.searched != "from:alice has:attachment" {
		t.Fatalf("query not threaded: %q", f.provider.searched)
	}
	if f.provider.searchLimit != 50 {
		t.Fatalf("limit not threaded: %d", f.provider.searchLimit)
	}
	threads, _ := out["threads"].([]map[string]interface{})
	if len(threads) != 2 || threads[0]["id"] != "t1" {
		t.Fatalf("threads not surfaced: %+v", out)
	}
}

func TestReadThread_RoutesAndSurfacesBodies(t *testing.T) {
	f := newFixture(t)
	f.provider.readOut = Thread{
		ID:      "thread-x",
		Subject: "Project status",
		Messages: []Message{{
			ID: "m1", From: "alice@example.com", Subject: "Project status",
			BodyText: "all good", BodyHTML: "<p>all good</p>",
		}},
	}
	read := f.toolByName("gmail.read_thread")
	out, err := read(map[string]interface{}{
		"connection_id": f.connectionID,
		"thread_id":     "thread-x",
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if f.provider.readID != "thread-x" {
		t.Fatalf("thread_id not threaded: %q", f.provider.readID)
	}
	if out["subject"] != "Project status" {
		t.Fatalf("subject missing")
	}
	msgs := out["messages"].([]map[string]interface{})
	if len(msgs) != 1 || msgs[0]["body_text"] != "all good" {
		t.Fatalf("message body missing: %+v", msgs)
	}
}

func TestLabel_RequiresAddOrRemove(t *testing.T) {
	f := newFixture(t)
	label := f.toolByName("gmail.label")
	_, err := label(map[string]interface{}{
		"connection_id": f.connectionID,
		"message_id":    "m1",
	})
	if err == nil {
		t.Fatal("expected error when both add + remove are empty")
	}
}

func TestLabel_PassesAddAndRemove(t *testing.T) {
	f := newFixture(t)
	label := f.toolByName("gmail.label")
	if _, err := label(map[string]interface{}{
		"connection_id": f.connectionID,
		"message_id":    "m1",
		"add":           []interface{}{"STARRED"},
		"remove":        []interface{}{"INBOX"},
	}); err != nil {
		t.Fatalf("label: %v", err)
	}
	if f.provider.labeledID != "m1" {
		t.Fatalf("message_id drift")
	}
	if len(f.provider.labelAdd) != 1 || f.provider.labelAdd[0] != "STARRED" {
		t.Fatalf("add slice drift: %v", f.provider.labelAdd)
	}
	if len(f.provider.labelRemove) != 1 || f.provider.labelRemove[0] != "INBOX" {
		t.Fatalf("remove slice drift: %v", f.provider.labelRemove)
	}
}

func TestArchive_DelegatesToProvider(t *testing.T) {
	f := newFixture(t)
	arch := f.toolByName("gmail.archive")
	if _, err := arch(map[string]interface{}{
		"connection_id": f.connectionID,
		"message_id":    "m1",
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if f.provider.archivedID != "m1" {
		t.Fatalf("archive id drift")
	}
}

func TestTools_RefuseUnboundConnection(t *testing.T) {
	f := newFixture(t)
	send := f.toolByName("gmail.send")
	_, err := send(map[string]interface{}{
		"connection_id":  f.connectionID,
		"__assistant_id": "asst-no-binding",
		"to":             "x@example.com",
		"subject":        "x",
		"body":           "x",
	})
	if err == nil {
		t.Fatal("expected ConnectionNotBoundError")
	}
	if !errors.Is(err, plugins.ErrConnectionNotBound) {
		t.Fatalf("expected ErrConnectionNotBound, got %v", err)
	}
}

func TestTools_RefuseDisabledConnection(t *testing.T) {
	f := newFixture(t)
	c, _ := f.connsRepo.GetByID(f.connectionID)
	c.Enabled = false
	if err := f.connsRepo.Update(c); err != nil {
		t.Fatalf("disable connection: %v", err)
	}

	send := f.toolByName("gmail.send")
	_, err := send(map[string]interface{}{
		"connection_id": f.connectionID,
		"to":            "x@example.com",
		"subject":       "x",
		"body":          "x",
	})
	if err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("expected disabled-connection error, got %v", err)
	}
}

// --- buildRFC822 ----------------------------------------------

func TestBuildRFC822_PlainText(t *testing.T) {
	body, err := buildRFC822(SendOptions{
		To:      []string{"alice@example.com"},
		Subject: "Hi",
		Body:    "hello world",
	})
	if err != nil {
		t.Fatalf("buildRFC822: %v", err)
	}
	if !strings.Contains(string(body), "To: alice@example.com") {
		t.Fatal("missing To header")
	}
	if !strings.Contains(string(body), "Subject: Hi") {
		t.Fatal("missing Subject header")
	}
	if !strings.Contains(string(body), `text/plain; charset="utf-8"`) {
		t.Fatal("plaintext content-type missing")
	}
}

func TestBuildRFC822_HTMLProducesAlternative(t *testing.T) {
	body, err := buildRFC822(SendOptions{
		To:      []string{"alice@example.com"},
		Subject: "Hi",
		Body:    "plain version",
		HTML:    "<p>html version</p>",
	})
	if err != nil {
		t.Fatalf("buildRFC822: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "multipart/alternative") {
		t.Fatal("HTML should produce multipart/alternative")
	}
	if !strings.Contains(s, "plain version") || !strings.Contains(s, "html version") {
		t.Fatal("both bodies must appear in the message")
	}
}

func TestBuildRFC822_AttachmentProducesMixed(t *testing.T) {
	body, err := buildRFC822(SendOptions{
		To:      []string{"alice@example.com"},
		Subject: "Hi",
		Body:    "see attached",
		Attachments: []Attachment{
			{Filename: "note.txt", ContentType: "text/plain", Data: []byte("payload")},
		},
	})
	if err != nil {
		t.Fatalf("buildRFC822: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "multipart/mixed") {
		t.Fatal("attachment should produce multipart/mixed")
	}
	if !strings.Contains(s, `filename="note.txt"`) {
		t.Fatal("attachment filename missing")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("payload"))
	if !strings.Contains(s, encoded) {
		t.Fatal("attachment payload not base64-encoded into message")
	}
}

func TestBuildRFC822_RejectsEmptyTo(t *testing.T) {
	_, err := buildRFC822(SendOptions{Subject: "x", Body: "y"})
	if err == nil {
		t.Fatal("expected error for empty To")
	}
}
