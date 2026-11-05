package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
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
)

// newTestVerifier returns a verifier trusting a fresh key, and a function
// signing claims with it.
func newTestVerifier(t *testing.T) (*Verifier, func(map[string]any) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	v := &Verifier{verifier: oidc.NewVerifier(testIssuer, keys, &oidc.Config{ClientID: testClientId})}
	sign := func(c map[string]any) string {
		t.Helper()
		raw, err := jwt.Signed(signer).Claims(c).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	return v, sign
}

// tokenClaims are the claims authentik puts in a token for a person who is an
// officer of epic and a member of other, with extra overriding them.
func tokenClaims(extra map[string]any) map[string]any {
	c := map[string]any{
		"iss":                testIssuer,
		"aud":                testClientId,
		"sub":                "hashed-user-id",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Unix(),
		"preferred_username": "olive",
		"campaigns":          map[string][]string{"epic": {"officer"}, "other": {"mapper"}},
		"mapper_admin":       false,
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func TestCaller(t *testing.T) {
	v, sign := newTestVerifier(t)

	got, err := v.Caller(context.Background(), sign(tokenClaims(nil)))
	if err != nil {
		t.Fatalf("Caller = %v", err)
	}
	want := campaign.Caller{
		Subject:  "hashed-user-id",
		Username: "olive",
		Roles: map[campaign.Slug][]campaign.Role{
			"epic":  {campaign.RoleOfficer},
			"other": {campaign.RoleMapper},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Caller = %+v, want %+v", got, want)
	}
}

// TestCallerDropsUnknownRoles checks a role the service does not know grants
// nothing, rather than being passed along.
func TestCallerDropsUnknownRoles(t *testing.T) {
	v, sign := newTestVerifier(t)
	raw := sign(tokenClaims(map[string]any{"campaigns": map[string][]string{"epic": {"owner", "mapper"}}, "mapper_admin": true}))

	got, err := v.Caller(context.Background(), raw)
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

func TestCallerRejects(t *testing.T) {
	v, sign := newTestVerifier(t)
	_, otherSign := newTestVerifier(t)
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
			if err == nil {
				t.Fatal("Caller = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Caller = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
