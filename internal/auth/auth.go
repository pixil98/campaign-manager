// Package auth works out who is calling from their authentik token.
package auth

import (
	"context"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/pixil98/campaign-manager/internal/campaign"
)

// Verifier checks tokens authentik issued for this service's provider and
// reads the caller from them. authentik's access tokens are signed JWTs
// carrying the same claims as its ID tokens, including those from scope
// mappings, so the provider needs include_claims_in_id_token.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewVerifier discovers issuer's signing keys and returns a verifier for
// tokens issued to clientId.
func NewVerifier(ctx context.Context, issuer, clientId string) (*Verifier, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discovering %s: %w", issuer, err)
	}
	return &Verifier{verifier: provider.Verifier(&oidc.Config{ClientID: clientId})}, nil
}

// claims are the token claims a caller is read from. Campaigns and
// MapperAdmin come from the same scope mapping as Mapper's, which builds
// them from the groups' attributes, the record of who is in what.
type claims struct {
	Username    string              `json:"preferred_username"`
	Campaigns   map[string][]string `json:"campaigns"`
	MapperAdmin bool                `json:"mapper_admin"`
}

// Caller verifies raw, its signature, issuer, audience and expiry, and
// returns who it belongs to.
func (v *Verifier) Caller(ctx context.Context, raw string) (campaign.Caller, error) {
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return campaign.Caller{}, fmt.Errorf("verifying token: %w", err)
	}
	var c claims
	if err := token.Claims(&c); err != nil {
		return campaign.Caller{}, fmt.Errorf("reading token claims: %w", err)
	}
	return callerFrom(token.Subject, c), nil
}

// callerFrom turns a token's claims into a caller. Roles it does not know
// are dropped, so a claim can never grant more than the roles there are.
func callerFrom(subject string, c claims) campaign.Caller {
	caller := campaign.Caller{
		Subject:  subject,
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
