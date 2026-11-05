// Package auth works out who is calling from their authentik token.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/pixil98/campaign-manager/internal/campaign"
)

// ErrInvalidToken is a token that is no good: not one authentik issued to
// this service, or one authentik no longer stands behind, because it expired,
// was revoked, or its user was deactivated. Any other error from Caller means
// authentik could not be asked, and says nothing about the token.
var ErrInvalidToken = errors.New("invalid token")

// Verifier checks tokens authentik issued for this service's provider and
// asks authentik who they belong to.
//
// A token is checked here first, its signature, issuer, audience and expiry,
// since authentik's userinfo endpoint answers for every provider's tokens.
// authentik's userinfo answer then confirms it still stands behind the
// token, so a revoked token stops working at once, and gives the caller's
// roles as they are now rather than as they were when the token was issued.
type Verifier struct {
	verifier    *oidc.IDTokenVerifier
	userInfoURL string
	hc          *http.Client
}

// NewVerifier discovers issuer's signing keys and userinfo endpoint, and
// returns a verifier for tokens issued to clientId that asks authentik about
// them with hc.
func NewVerifier(ctx context.Context, issuer, clientId string, hc *http.Client) (*Verifier, error) {
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, hc), issuer)
	if err != nil {
		return nil, fmt.Errorf("discovering %s: %w", issuer, err)
	}
	if provider.UserInfoEndpoint() == "" {
		return nil, fmt.Errorf("%s has no userinfo endpoint", issuer)
	}
	return &Verifier{
		verifier:    provider.Verifier(&oidc.Config{ClientID: clientId}),
		userInfoURL: provider.UserInfoEndpoint(),
		hc:          hc,
	}, nil
}

// Scopes are the scopes a token needs for Caller to read the caller from it:
// profile brings preferred_username, and campaigns brings campaigns and
// mapper_admin.
var Scopes = []string{oidc.ScopeOpenID, "profile", "campaigns"}

// claims are the userinfo claims a caller is read from. Campaigns and
// MapperAdmin come from the same scope mapping as Mapper's, which builds
// them from the groups' attributes, the record of who is in what.
type claims struct {
	Subject     string              `json:"sub"`
	Username    string              `json:"preferred_username"`
	Campaigns   map[string][]string `json:"campaigns"`
	MapperAdmin bool                `json:"mapper_admin"`
}

// Caller verifies raw and returns who it belongs to, as authentik describes
// them now.
func (v *Verifier) Caller(ctx context.Context, raw string) (campaign.Caller, error) {
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return campaign.Caller{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	c, err := v.userInfo(ctx, raw)
	if err != nil {
		return campaign.Caller{}, err
	}
	// OpenID Connect requires this, so that an answer about someone else
	// can never be taken for the token's own.
	if c.Subject != token.Subject {
		return campaign.Caller{}, fmt.Errorf("%w: authentik answered for subject %q, not the token's %q", ErrInvalidToken, c.Subject, token.Subject)
	}
	return callerFrom(c), nil
}

// userInfo asks authentik's userinfo endpoint about raw. authentik answers
// 401 for a token it no longer has, and 403 for one without the openid
// scope.
func (v *Verifier) userInfo(ctx context.Context, raw string) (claims, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.userInfoURL, nil)
	if err != nil {
		return claims{}, err
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := v.hc.Do(req)
	if err != nil {
		return claims{}, fmt.Errorf("asking authentik about the token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return claims{}, fmt.Errorf("%w: authentik refused it: %s", ErrInvalidToken, resp.Header.Get("WWW-Authenticate"))
	default:
		return claims{}, fmt.Errorf("asking authentik about the token: %s", resp.Status)
	}
	var c claims
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil {
		return claims{}, fmt.Errorf("reading authentik's userinfo: %w", err)
	}
	return c, nil
}

// callerFrom turns userinfo claims into a caller. Roles it does not know are
// dropped, so a claim can never grant more than the roles there are.
func callerFrom(c claims) campaign.Caller {
	caller := campaign.Caller{
		Subject:  c.Subject,
		Username: c.Username,
		Admin:    c.MapperAdmin,
		Roles:    map[campaign.Slug][]campaign.Role{},
	}
	for slug, roles := range c.Campaigns {
		for _, r := range roles {
			if role := campaign.Role(r); role.Valid() {
				caller.Roles[campaign.Slug(slug)] = append(caller.Roles[campaign.Slug(slug)], role)
			}
		}
	}
	return caller
}
