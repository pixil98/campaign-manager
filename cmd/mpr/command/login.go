package command

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/pixil98/campaign-manager/internal/client"
)

func newLoginCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in to a campaign-manager server",
		Long: `Log in to a campaign-manager server, through authentik in a browser.

mpr prints a link to open, on this machine or any other, and waits while you
log in there. The login lasts until authentik's refresh token runs out, or
until "mpr logout". The first server you log in to becomes your default.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.login(cmd)
		},
	}
}

func (a *app) login(cmd *cobra.Command) error {
	path, err := a.configFile()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	server, err := a.serverURL(cfg)
	if err != nil {
		return err
	}
	ctx := oidc.ClientContext(cmd.Context(), a.hc)

	info, err := client.New(server, a.hc).Login(ctx)
	if err != nil {
		return fmt.Errorf("asking %s how to log in: %w", server, err)
	}
	provider, err := oidc.NewProvider(ctx, info.Issuer)
	if err != nil {
		return fmt.Errorf("discovering %s: %w", info.Issuer, err)
	}
	oc := &oauth2.Config{
		ClientID: info.ClientId,
		Endpoint: provider.Endpoint(),
		// offline_access brings a refresh token, so a login outlasts its
		// short-lived access token.
		Scopes: append(slices.Clone(info.Scopes), oidc.ScopeOfflineAccess),
	}

	auth, err := oc.DeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("starting the login: %w", err)
	}
	link := auth.VerificationURIComplete
	if link == "" {
		link = auth.VerificationURI
	}
	stderr := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(stderr, "To log in, open this link and check it shows the code %s:\n\n    %s\n\nWaiting for you to log in...\n", auth.UserCode, link)
	token, err := oc.DeviceAccessToken(ctx, auth)
	if err != nil {
		return fmt.Errorf("logging in: %w", deviceLoginError(cmd.Context(), err))
	}

	// Asking who the token is for checks the server takes it before it is
	// saved.
	me, err := client.New(server, withTimeout(oc.Client(ctx, token), a.hc)).Me(ctx)
	if err != nil {
		return fmt.Errorf("checking the login with %s: %w", server, err)
	}

	credsPath := credentialsFile(path)
	creds, err := loadCredentials(credsPath)
	if err != nil {
		return err
	}
	creds[server] = &savedLogin{Issuer: info.Issuer, ClientId: info.ClientId, TokenURL: oc.Endpoint.TokenURL, Token: token}
	if err := creds.save(credsPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "Logged in to %s as %s.\n", server, me.Username)

	if cfg.Server == "" {
		cfg.Server = server
		if err := cfg.save(path); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stderr, "%s is now your default server.\n", server)
	}
	return nil
}

// deviceLoginError explains why a device login ended without a token. ctx is
// the command's own, which is only done when the command was interrupted.
func deviceLoginError(ctx context.Context, err error) error {
	var refused *oauth2.RetrieveError
	errors.As(err, &refused)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.DeadlineExceeded) || (refused != nil && refused.ErrorCode == "expired_token"):
		// The device code's own expiry, which DeviceAccessToken waits until.
		return errors.New(`the link expired before the login was finished; run "mpr login" again`)
	case refused != nil && refused.ErrorCode == "access_denied":
		return errors.New("the login was declined")
	}
	return err
}

func newLogoutCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out of a campaign-manager server",
		Long: `Log out of a campaign-manager server. The login's tokens are revoked at
authentik as well as forgotten here, so a copy of them is no use either.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.logout(cmd)
		},
	}
}

func (a *app) logout(cmd *cobra.Command) error {
	path, err := a.configFile()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	server, err := a.serverURL(cfg)
	if err != nil {
		return err
	}
	credsPath := credentialsFile(path)
	creds, err := loadCredentials(credsPath)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	login, ok := creds[server]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "Not logged in to %s.\n", server)
		return nil
	}

	if err := a.revoke(cmd.Context(), login); err != nil {
		_, _ = fmt.Fprintf(stderr, "Could not revoke the login at authentik, so it is only forgotten here: %v\n", err)
	}
	delete(creds, server)
	if err := creds.save(credsPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "Logged out of %s.\n", server)
	return nil
}

// revoke ends login at authentik: the refresh token, so it cannot be used
// for new access tokens, and the access token, so it stops working now
// rather than when it expires.
func (a *app) revoke(ctx context.Context, login *savedLogin) error {
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, a.hc), login.Issuer)
	if err != nil {
		return fmt.Errorf("discovering %s: %w", login.Issuer, err)
	}
	var meta struct {
		RevocationURL string `json:"revocation_endpoint"`
	}
	if err := provider.Claims(&meta); err != nil {
		return err
	}
	if meta.RevocationURL == "" {
		return errors.New("authentik does not offer revocation")
	}

	for _, t := range []struct{ hint, token string }{
		{"refresh_token", login.Token.RefreshToken},
		{"access_token", login.Token.AccessToken},
	} {
		if t.token == "" {
			continue
		}
		form := url.Values{"token": {t.token}, "token_type_hint": {t.hint}, "client_id": {login.ClientId}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.RevocationURL, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := a.hc.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("revoking the %s: authentik answered %s", strings.ReplaceAll(t.hint, "_", " "), resp.Status)
		}
	}
	return nil
}

// apiClient returns a client for the server, which sends the saved login's
// token, refreshing it as it runs out and saving the refreshed one.
func (a *app) apiClient(ctx context.Context) (*client.Client, string, error) {
	path, err := a.configFile()
	if err != nil {
		return nil, "", err
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, "", err
	}
	server, err := a.serverURL(cfg)
	if err != nil {
		return nil, "", err
	}
	credsPath := credentialsFile(path)
	creds, err := loadCredentials(credsPath)
	if err != nil {
		return nil, "", err
	}
	login, ok := creds[server]
	if !ok || login.Token == nil {
		return nil, "", fmt.Errorf(`not logged in to %s: run "mpr login"`, server)
	}

	ctx = oidc.ClientContext(ctx, a.hc)
	oc := &oauth2.Config{ClientID: login.ClientId, Endpoint: oauth2.Endpoint{TokenURL: login.TokenURL}}
	src := &savingSource{
		src:  oc.TokenSource(ctx, login.Token),
		last: login.Token.AccessToken,
		save: func(t *oauth2.Token) error {
			login.Token = t
			return creds.save(credsPath)
		},
	}
	return client.New(server, withTimeout(oauth2.NewClient(ctx, src), a.hc)), server, nil
}

// withTimeout gives hc, an HTTP client from oauth2, the timeout of base,
// which oauth2 does not carry over.
func withTimeout(hc, base *http.Client) *http.Client {
	hc.Timeout = base.Timeout
	return hc
}

// explain turns an error that means the login is no good any more into
// advice to log in again.
func explain(server string, err error) error {
	var refresh *oauth2.RetrieveError
	if errors.As(err, &refresh) {
		return fmt.Errorf(`your login to %s has run out; run "mpr login" again: %w`, server, err)
	}
	var apiErr *client.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		return fmt.Errorf(`%s did not take your login; run "mpr login" again: %w`, server, err)
	}
	return err
}
