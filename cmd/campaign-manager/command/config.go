package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds the service's configuration, read from the file given with
// --config.
type Config struct {
	// Listen is the address the API listens on, such as ":8080".
	Listen    string          `json:"listen"`
	OIDC      OIDCConfig      `json:"oidc"`
	Authentik AuthentikConfig `json:"authentik"`
	Wiki      WikiConfig      `json:"wiki"`
	// ReconcileInterval is how often invitations for people who have an
	// account by now are settled.
	ReconcileInterval Duration `json:"reconcile_interval"`
}

// Validate reports every problem found in the configuration at once.
func (c *Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New(`listen must be set, such as ":8080"`))
	}
	if err := c.OIDC.validate(); err != nil {
		errs = append(errs, fmt.Errorf("oidc: %w", err))
	}
	if err := c.Authentik.validate(); err != nil {
		errs = append(errs, fmt.Errorf("authentik: %w", err))
	}
	if err := c.Wiki.validate(); err != nil {
		errs = append(errs, fmt.Errorf("wiki: %w", err))
	}
	if c.ReconcileInterval <= 0 {
		errs = append(errs, errors.New(`reconcile_interval must be set to a length of time, such as "1m"`))
	}
	return errors.Join(errs...)
}

// OIDCConfig is the authentik provider callers' tokens are issued by, under
// "oidc".
type OIDCConfig struct {
	// Issuer is the provider's issuer, such as
	// https://auth.example.com/application/o/campaign-manager/.
	Issuer string `json:"issuer"`
	// ClientId is the provider's client id, which tokens must be issued to.
	ClientId string `json:"client_id"`
}

func (c OIDCConfig) validate() error {
	var errs []error
	if err := checkURL("issuer", c.Issuer); err != nil {
		errs = append(errs, err)
	}
	if c.ClientId == "" {
		errs = append(errs, errors.New("client_id must be set"))
	}
	return errors.Join(errs...)
}

// AuthentikConfig is how the service reaches authentik and what it makes
// there, under "authentik".
type AuthentikConfig struct {
	// URL is authentik's public address. It must be the public one: the
	// links in invitation emails are built from the host they are asked
	// for on.
	URL string `json:"url"`
	// Token is an API token for the service's own account, or TokenFile a
	// file holding it, so the token can come from a mounted Secret instead
	// of the config. Exactly one is set.
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	// BaseGroup is the group every campaign group sits under, and the
	// first part of their names.
	BaseGroup string `json:"base_group"`
	// InvitationFlow is the slug of the enrollment flow invitations use.
	InvitationFlow string `json:"invitation_flow"`
	// InvitationLifetime is how long an invitation can be redeemed for.
	InvitationLifetime Duration `json:"invitation_lifetime"`
	// SendInvitationEmail has authentik email invitations. Off, the link
	// is only returned, for the inviter to pass on.
	SendInvitationEmail bool `json:"send_invitation_email"`
}

func (c AuthentikConfig) validate() error {
	var errs []error
	if err := checkURL("url", c.URL); err != nil {
		errs = append(errs, err)
	}
	if err := checkToken(c.Token, c.TokenFile); err != nil {
		errs = append(errs, err)
	}
	if c.BaseGroup == "" {
		errs = append(errs, errors.New("base_group must be set"))
	}
	if c.InvitationFlow == "" {
		errs = append(errs, errors.New("invitation_flow must be set"))
	}
	if c.InvitationLifetime <= 0 {
		errs = append(errs, errors.New(`invitation_lifetime must be set to a length of time, such as "168h"`))
	}
	return errors.Join(errs...)
}

// WikiConfig is how the service reaches the wiki, under "wiki".
type WikiConfig struct {
	// URL is the wiki's address. Its in-cluster service will do.
	URL string `json:"url"`
	// Token is an API token for the service's wiki account, which has to
	// be a superuser to add ACL rules, or TokenFile a file holding it.
	// Exactly one is set.
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	// TemplatesNamespace is where new campaigns' working pages are copied
	// from.
	TemplatesNamespace string `json:"templates_namespace"`
}

func (c WikiConfig) validate() error {
	var errs []error
	if err := checkURL("url", c.URL); err != nil {
		errs = append(errs, err)
	}
	if err := checkToken(c.Token, c.TokenFile); err != nil {
		errs = append(errs, err)
	}
	if c.TemplatesNamespace == "" {
		errs = append(errs, errors.New("templates_namespace must be set"))
	}
	return errors.Join(errs...)
}

// checkToken reports a token that is not given exactly one way.
func checkToken(token, file string) error {
	switch {
	case token == "" && file == "":
		return errors.New("token or token_file must be set")
	case token != "" && file != "":
		return errors.New("only one of token and token_file can be set")
	}
	return nil
}

// readToken returns the token, from the config or from its file. A file's
// surrounding whitespace, such as a trailing newline, is not part of it.
func readToken(token, file string) (string, error) {
	if file == "" {
		return token, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading token: %w", err)
	}
	token = strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", file)
	}
	return token, nil
}

// checkURL reports a setting that is not an http or https URL.
func checkURL(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s must be set", name)
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s %q must be an http or https URL", name, value)
	}
	return nil
}

// Duration is a length of time in the config, written as Go writes one,
// such as "1m" or "168h".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf(`a length of time is written as a string, such as "1m": %w`, err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
