package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/pixil98/campaign-manager/internal/campaign"
)

const (
	testIssuer   = "https://auth.example.com/application/o/campaign-manager/"
	testClientId = "campaign-manager"
	testSubject  = "hashed-user-id"
)

// fakeUserInfo is authentik's userinfo endpoint, answering status and, for
// 200, claims.
type fakeUserInfo struct {
	*httptest.Server
	status int
	claims map[string]any
	tokens []string // the bearer tokens it was asked about
}

// newTestVerifier returns a verifier trusting a fresh key and asking a fake
// userinfo endpoint, which describes olive as an officer of epic and a
// mapper of other, and a function signing token claims with the key.
func newTestVerifier(t *testing.T) (*Verifier, func(map[string]any) string, *fakeUserInfo) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(c map[string]any) string {
		t.Helper()
		raw, err := jwt.Signed(signer).Claims(c).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	ui := &fakeUserInfo{status: http.StatusOK, claims: map[string]any{
		"sub":                testSubject,
		"preferred_username": "olive",
		"campaigns":          map[string][]string{"epic": {"officer"}, "other": {"mapper"}},
		"mapper_admin":       false,
	}}
	ui.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ui.tokens = append(ui.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if ui.status != http.StatusOK {
			w.Header().Set("WWW-Authenticate", `error="invalid_token"`)
			w.WriteHeader(ui.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ui.claims)
	}))
	t.Cleanup(ui.Close)

	keys := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	v := &Verifier{
		verifier:    oidc.NewVerifier(testIssuer, keys, &oidc.Config{ClientID: testClientId}),
		userInfoURL: ui.URL,
		hc:          ui.Client(),
	}
	return v, sign, ui
}

// tokenClaims are the claims of a token authentik issued to olive, with
// extra overriding them.
func tokenClaims(extra map[string]any) map[string]any {
	c := map[string]any{
		"iss": testIssuer,
		"aud": testClientId,
		"sub": testSubject,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func TestCaller(t *testing.T) {
	v, sign, ui := newTestVerifier(t)
	// Roles in the token itself are as they were when it was issued, and
	// are never read.
	raw := sign(tokenClaims(map[string]any{"campaigns": map[string][]string{"gone": {"officer"}}, "mapper_admin": true}))

	got, err := v.Caller(context.Background(), raw)
	if err != nil {
		t.Fatalf("Caller = %v", err)
	}
	want := campaign.Caller{
		Subject:  testSubject,
		Username: "olive",
		Roles: map[campaign.Slug][]campaign.Role{
			"epic":  {campaign.RoleOfficer},
			"other": {campaign.RoleMapper},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Caller = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(ui.tokens, []string{raw}) {
		t.Errorf("userinfo was asked about %q, want just the token", ui.tokens)
	}
}

// TestCallerDropsUnknownRoles checks a role the service does not know grants
// nothing, rather than being passed along.
func TestCallerDropsUnknownRoles(t *testing.T) {
	v, sign, ui := newTestVerifier(t)
	ui.claims["campaigns"] = map[string][]string{"epic": {"owner", "mapper"}}
	ui.claims["mapper_admin"] = true

	got, err := v.Caller(context.Background(), sign(tokenClaims(nil)))
	if err != nil {
		t.Fatalf("Caller = %v", err)
	}
	if !got.Admin {
		t.Error("Admin = false, want true from mapper_admin")
	}
	if want := []campaign.Role{campaign.RoleMapper}; !reflect.DeepEqual(got.Roles["epic"], want) {
		t.Errorf("epic roles = %v, want %v", got.Roles["epic"], want)
	}
}

// TestCallerRejects checks tokens that are no good never reach authentik.
func TestCallerRejects(t *testing.T) {
	v, sign, ui := newTestVerifier(t)
	_, otherSign, _ := newTestVerifier(t)
	tests := map[string]struct {
		token   string
		wantErr string
	}{
		"issued to another client": {
			token:   sign(tokenClaims(map[string]any{"aud": "mapper"})),
			wantErr: "audience",
		},
		"from another issuer": {
			token:   sign(tokenClaims(map[string]any{"iss": "https://auth.example.com/application/o/mapper/"})),
			wantErr: "issued by a different provider",
		},
		"expired": {
			token:   sign(tokenClaims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})),
			wantErr: "expired",
		},
		"signed with another key": {
			token:   otherSign(tokenClaims(nil)),
			wantErr: "signature",
		},
		"not a token": {
			token:   "nonsense",
			wantErr: "malformed",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := v.Caller(context.Background(), tt.token)
			if !errors.Is(err, ErrInvalidToken) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Caller = %v, want ErrInvalidToken mentioning %q", err, tt.wantErr)
			}
		})
	}
	if len(ui.tokens) != 0 {
		t.Errorf("userinfo was asked about %d tokens, want none", len(ui.tokens))
	}
}

// TestCallerAsksAuthentik checks a good token that authentik no longer
// stands behind is invalid, and that authentik failing to answer is not.
func TestCallerAsksAuthentik(t *testing.T) {
	tests := map[string]struct {
		status      int
		subject     string
		unreachable bool
		wantInvalid bool
	}{
		"revoked":                   {status: http.StatusUnauthorized, wantInvalid: true},
		"without the openid scope":  {status: http.StatusForbidden, wantInvalid: true},
		"answered for someone else": {status: http.StatusOK, subject: "someone-else", wantInvalid: true},
		"authentik is failing":      {status: http.StatusBadGateway},
		"authentik is unreachable":  {status: http.StatusOK, unreachable: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			v, sign, ui := newTestVerifier(t)
			ui.status = tt.status
			if tt.subject != "" {
				ui.claims["sub"] = tt.subject
			}
			if tt.unreachable {
				ui.Close()
			}

			_, err := v.Caller(context.Background(), sign(tokenClaims(nil)))
			if err == nil {
				t.Fatal("Caller = nil, want an error")
			}
			if got := errors.Is(err, ErrInvalidToken); got != tt.wantInvalid {
				t.Errorf("Caller = %v; ErrInvalidToken is %t, want %t", err, got, tt.wantInvalid)
			}
		})
	}
}
