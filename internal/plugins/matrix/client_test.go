package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTextBody(t *testing.T) {
	body, ok := parseTextBody(json.RawMessage(`{"msgtype":"m.text","body":"hello"}`))
	if !ok || body != "hello" {
		t.Fatalf("got %q ok=%v", body, ok)
	}
	if _, ok := parseTextBody(json.RawMessage(`{"msgtype":"m.image","body":"x"}`)); ok {
		t.Fatal("image should be skipped")
	}
}

func TestParseReaction(t *testing.T) {
	raw := json.RawMessage(`{"m.relates_to":{"rel_type":"m.annotation","event_id":"$abc","key":"✅"}}`)
	eid, key, ok := parseReaction(raw)
	if !ok || eid != "$abc" || key != "✅" {
		t.Fatalf("got %q %q ok=%v", eid, key, ok)
	}
}

func TestClientWhoamiAndSend(t *testing.T) {
	var gotAuth string
	var sentBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/account/whoami"):
			_, _ = w.Write([]byte(`{"user_id":"@bot:example.com"}`))
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			_ = json.NewDecoder(r.Body).Decode(&sentBody)
			_, _ = w.Write([]byte(`{"event_id":"$evt1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cli := newClient(srv.URL, "syt_token")
	uid, err := cli.whoami(t.Context())
	if err != nil || uid != "@bot:example.com" {
		t.Fatalf("whoami: %v %q", err, uid)
	}
	if gotAuth != "Bearer syt_token" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	eid, err := cli.sendText(t.Context(), "!room:example.com", "hi")
	if err != nil || eid != "$evt1" {
		t.Fatalf("send: %v %q", err, eid)
	}
	if sentBody["body"] != "hi" || sentBody["msgtype"] != "m.text" {
		t.Fatalf("payload: %#v", sentBody)
	}
}
