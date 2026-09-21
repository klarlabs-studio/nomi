package webhooks

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestChooseVerifierTeams(t *testing.T) {
	if _, ok := chooseVerifier("com.nomi.teams").(*teamsVerifier); !ok {
		t.Fatal("teams")
	}
}

func TestTeamsVerifierValidJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const appID = "app-123"
	teamsKeyFunc = func(tok *jwt.Token) (interface{}, error) {
		return &key.PublicKey, nil
	}
	t.Cleanup(func() { teamsKeyFunc = defaultTeamsKeyFunc })

	claims := jwt.MapClaims{
		"iss": "https://api.botframework.com",
		"aud": appID,
		"exp": time.Now().Add(time.Hour).Unix(),
		"nbf": time.Now().Add(-time.Minute).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "test"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	v := &teamsVerifier{}
	if err := v.Verify([]byte(`{"type":"message"}`), appID, map[string]string{
		"Authorization": "Bearer " + signed,
	}); err != nil {
		t.Fatalf("valid jwt: %v", err)
	}
	if err := v.Verify([]byte(`{}`), appID, map[string]string{
		"Authorization": "Bearer " + signed,
	}); err != nil {
		// body unused
		t.Fatalf("unexpected: %v", err)
	}
	if err := v.Verify([]byte(`{}`), "other-app", map[string]string{
		"Authorization": "Bearer " + signed,
	}); err == nil {
		t.Fatal("expected audience mismatch")
	}
	if err := v.Verify([]byte(`{}`), appID, nil); err == nil {
		t.Fatal("expected missing auth")
	}
	if got := v.EventType(nil, []byte(`{"type":"message"}`)); got != "teams_message" {
		t.Fatalf("event type: %q", got)
	}
}
