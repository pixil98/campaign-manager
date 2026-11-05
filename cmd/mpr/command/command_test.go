package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/pixil98/campaign-manager/internal/api"
	"github.com/pixil98/campaign-manager/internal/campaign"
)

const testClientId = "campaign-manager"

// fakeAuthentik is the authentik a test server sends logins to. A device
// login is approved at once, and each refresh replaces the refresh token,
// as authentik does.
type fakeAuthentik struct {
	*httptest.Server

	// deviceError, when set, is the token endpoint's answer to a device
	// login instead of a token, and deviceExpiresIn how long the login's
	// code lasts.
	deviceError     string
	deviceExpiresIn int

	mu       sync.Mutex
	issued   int
	refresh  map[string]bool // refresh tokens still good
	revoked  []string
	lastForm map[string]string
}

func newFakeAuthentik(t *testing.T) *fakeAuthentik {
	t.Helper()
	f := &fakeAuthentik{refresh: map[string]bool{}, deviceExpiresIn: 60}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{
			"issuer":                        f.URL,
			"authorization_endpoint":        f.URL + "/authorize",
			"device_authorization_endpoint": f.URL + "/device",
			"token_endpoint":                f.URL + "/token",
			"revocation_endpoint":           f.URL + "/revoke",
			"jwks_uri":                      f.URL + "/jwks",
		})
	})
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeTestJSON(w, http.StatusOK, map[string]any{
			"device_code":               "device-code",
			"user_code":                 "ABCD1234",
			"verification_uri":          f.URL + "/device-login",
			"verification_uri_complete": f.URL + "/device-login?code=ABCD1234",
			"expires_in":                f.deviceExpiresIn,
			"interval":                  1,
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.PostForm.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if r.PostForm.Get("device_code") != "device-code" {
				writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
				return
			}
			if f.deviceError != "" {
				writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": f.deviceError})
				return
			}
		case "refresh_token":
			if !f.refresh[r.PostForm.Get("refresh_token")] {
				writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
				return
			}
			delete(f.refresh, r.PostForm.Get("refresh_token"))
		default:
			writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
			return
		}
		f.issued++
		access, refresh := tokenNames(f.issued)
		f.refresh[refresh] = true
		writeTestJSON(w, http.StatusOK, map[string]any{
			"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 300,
		})
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.revoked = append(f.revoked, r.PostForm.Get("token"))
		writeTestJSON(w, http.StatusOK, map[string]any{})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// tokenNames are the n-th access and refresh tokens the fake hands out.
func tokenNames(n int) (access, refresh string) {
	return "access-" + string(rune('0'+n)), "refresh-" + string(rune('0'+n))
}

func (f *fakeAuthentik) record(r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastForm = map[string]string{}
	for k := range r.PostForm {
		f.lastForm[k] = r.PostForm.Get(k)
	}
}

// fakeAuth takes any access token the fake authentik hands out.
type fakeAuth struct{}

func (fakeAuth) Caller(_ context.Context, token string) (campaign.Caller, error) {
	if !strings.HasPrefix(token, "access-") {
		return campaign.Caller{}, errors.New("bad token")
	}
	return olive, nil
}

var olive = campaign.Caller{
	Username: "olive",
	Roles:    map[campaign.Slug][]campaign.Role{"dragons": {campaign.RoleOfficer}, "atlas": {campaign.RoleMapper}},
}

// fakeCampaigns answers with err if set, and records what it was asked.
type fakeCampaigns struct {
	err     error
	created *campaign.Created
	invited *campaign.Invited
	slug    string
	invite  campaign.InviteRequest
}

func (f *fakeCampaigns) Create(_ context.Context, _ campaign.Caller, req campaign.CreateRequest) (*campaign.Created, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.created != nil {
		return f.created, nil
	}
	return &campaign.Created{Slug: campaign.Slug(req.Slug), New: true, Groups: map[campaign.Role]string{
		campaign.RoleMapper: "mapper-" + req.Slug + "-mapper", campaign.RoleOfficer: "mapper-" + req.Slug + "-officer",
	}}, nil
}

func (f *fakeCampaigns) Invite(_ context.Context, _ campaign.Caller, slug string, req campaign.InviteRequest) (*campaign.Invited, error) {
	f.slug, f.invite = slug, req
	if f.err != nil {
		return nil, f.err
	}
	if f.invited != nil {
		return f.invited, nil
	}
	return &campaign.Invited{Status: campaign.StatusAdded}, nil
}

// testEnv is a campaign-manager and its authentik, and a config directory of
// mpr's own.
type testEnv struct {
	authentik *fakeAuthentik
	api       *httptest.Server
	campaigns *fakeCampaigns
	config    string
	env       map[string]string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{
		authentik: newFakeAuthentik(t),
		campaigns: &fakeCampaigns{},
		config:    filepath.Join(t.TempDir(), "mpr", "config.json"),
		env:       map[string]string{},
	}
	login := api.Login{Issuer: e.authentik.URL, ClientId: testClientId, Scopes: []string{"openid", "profile", "campaigns"}}
	e.api = httptest.NewServer(api.NewServer(":0", login, fakeAuth{}, e.campaigns).Handler())
	t.Cleanup(e.api.Close)
	return e
}

// run runs mpr with args, against the env's config file.
func (e *testEnv) run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd(&app{
		getenv: func(k string) string { return e.env[k] },
		hc:     &http.Client{Timeout: 10 * time.Second},
	})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--config", e.config}, args...))
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// loggedIn saves a login to the env's server, as mpr login would, with an
// access token that expires at expiry.
func (e *testEnv) loggedIn(t *testing.T, expiry time.Time) {
	t.Helper()
	e.authentik.mu.Lock()
	e.authentik.issued++
	access, refresh := tokenNames(e.authentik.issued)
	e.authentik.refresh[refresh] = true
	e.authentik.mu.Unlock()

	creds := credentials{e.api.URL: {
		Issuer: e.authentik.URL, ClientId: testClientId, TokenURL: e.authentik.URL + "/token",
		Token: &oauth2.Token{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", Expiry: expiry},
	}}
	if err := creds.save(credentialsFile(e.config)); err != nil {
		t.Fatal(err)
	}
	if err := (&Config{Server: e.api.URL}).save(e.config); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) savedLogin(t *testing.T) *savedLogin {
	t.Helper()
	creds, err := loadCredentials(credentialsFile(e.config))
	if err != nil {
		t.Fatal(err)
	}
	return creds[e.api.URL]
}

func TestLogin(t *testing.T) {
	e := newTestEnv(t)

	_, stderr, err := e.run(t, "login", "--server", e.api.URL+"/")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, stderr)
	}
	for _, want := range []string{"ABCD1234", e.authentik.URL + "/device-login?code=ABCD1234", "Logged in to " + e.api.URL + " as olive", "now your default"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("login said:\n%s\nwant it to contain %q", stderr, want)
		}
	}
	if got, want := e.authentik.lastForm["scope"], "openid profile campaigns offline_access"; got != want {
		t.Errorf("token request scope = %q, want %q", got, want)
	}

	login := e.savedLogin(t)
	if login == nil || login.Token.RefreshToken != "refresh-1" || login.TokenURL != e.authentik.URL+"/token" {
		t.Fatalf("saved login = %+v, want refresh-1 from %s/token", login, e.authentik.URL)
	}
	info, err := os.Stat(credentialsFile(e.config))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials are mode %o, want 600", perm)
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != e.api.URL {
		t.Errorf("default server = %q, want %q", cfg.Server, e.api.URL)
	}
}

func TestLoginNotFinished(t *testing.T) {
	tests := map[string]struct {
		deviceError     string
		deviceExpiresIn int
		wantErr         string
	}{
		"the link expired": {
			deviceError: "authorization_pending", deviceExpiresIn: 2,
			wantErr: `the link expired before the login was finished; run "mpr login" again`,
		},
		"authentik says it expired": {
			deviceError: "expired_token", deviceExpiresIn: 60,
			wantErr: `the link expired before the login was finished`,
		},
		"declined": {
			deviceError: "access_denied", deviceExpiresIn: 60,
			wantErr: "the login was declined",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.authentik.deviceError, e.authentik.deviceExpiresIn = tt.deviceError, tt.deviceExpiresIn

			_, _, err := e.run(t, "login", "--server", e.api.URL)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("login error = %v, want it to contain %q", err, tt.wantErr)
			}
			if e.savedLogin(t) != nil {
				t.Error("a login was saved")
			}
		})
	}
}

func TestLoginKeepsTheDefault(t *testing.T) {
	e := newTestEnv(t)
	if err := (&Config{Server: "https://campaigns.example.com"}).save(e.config); err != nil {
		t.Fatal(err)
	}

	if _, stderr, err := e.run(t, "login", "--server", e.api.URL); err != nil {
		t.Fatalf("login: %v\n%s", err, stderr)
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://campaigns.example.com" {
		t.Errorf("default server = %q, want it left alone", cfg.Server)
	}
	if e.savedLogin(t) == nil {
		t.Error("the login was not saved")
	}
}

func TestRefresh(t *testing.T) {
	e := newTestEnv(t)
	e.loggedIn(t, time.Now().Add(-time.Minute))

	stdout, stderr, err := e.run(t, "whoami")
	if err != nil {
		t.Fatalf("whoami: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "olive") {
		t.Errorf("whoami printed %q, want olive", stdout)
	}
	if login := e.savedLogin(t); login.Token.AccessToken != "access-2" || login.Token.RefreshToken != "refresh-2" {
		t.Errorf("saved tokens = %s, %s, want the refreshed access-2, refresh-2", login.Token.AccessToken, login.Token.RefreshToken)
	}

	// The refresh token was replaced, so only the saved one still works.
	if _, stderr, err := e.run(t, "whoami"); err != nil {
		t.Fatalf("whoami again: %v\n%s", err, stderr)
	}
}

func TestExpiredLogin(t *testing.T) {
	e := newTestEnv(t)
	e.loggedIn(t, time.Now().Add(-time.Minute))
	e.authentik.mu.Lock()
	clear(e.authentik.refresh)
	e.authentik.mu.Unlock()

	_, _, err := e.run(t, "whoami")
	if err == nil || !strings.Contains(err.Error(), `run "mpr login" again`) {
		t.Errorf("whoami error = %v, want advice to log in again", err)
	}
}

func TestLogout(t *testing.T) {
	e := newTestEnv(t)
	e.loggedIn(t, time.Now().Add(time.Hour))

	if _, stderr, err := e.run(t, "logout"); err != nil {
		t.Fatalf("logout: %v\n%s", err, stderr)
	}
	if got := strings.Join(e.authentik.revoked, " "); got != "refresh-1 access-1" {
		t.Errorf("revoked %q, want refresh-1 access-1", got)
	}
	if login := e.savedLogin(t); login != nil {
		t.Errorf("login still saved: %+v", login)
	}
	if _, _, err := e.run(t, "whoami"); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("whoami after logout error = %v, want not logged in", err)
	}
}

func TestCommands(t *testing.T) {
	expires := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		args       []string
		campaigns  *fakeCampaigns
		wantOut    []string
		wantErr    string
		wantSlug   string
		wantInvite campaign.InviteRequest
	}{
		"who am I": {
			args:    []string{"whoami"},
			wantOut: []string{"olive on http://", "atlas     mapper", "dragons   officer"},
		},
		"who am I, as JSON": {
			args:    []string{"whoami", "--json"},
			wantOut: []string{`"username": "olive"`, `"dragons": [`},
		},
		"a new campaign": {
			args:    []string{"campaign", "create", "dragons", "--name", "Here Be Dragons"},
			wantOut: []string{"Created campaign dragons.", "officer group: mapper-dragons-officer"},
		},
		"an existing campaign": {
			args:      []string{"campaign", "create", "dragons"},
			campaigns: &fakeCampaigns{created: &campaign.Created{Slug: "dragons", SeededPages: []string{"dragons:start"}}},
			wantOut:   []string{"already existed", "pages copied in: dragons:start"},
		},
		"not allowed": {
			args:      []string{"campaign", "create", "dragons"},
			campaigns: &fakeCampaigns{err: fmt.Errorf("%w: only an admin can create a campaign", campaign.ErrForbidden)},
			wantErr:   "only an admin can create a campaign (403 Forbidden)",
		},
		"someone with an account": {
			args:       []string{"invite", "dragons", "a@example.com", "--role", "officer"},
			wantOut:    []string{"a@example.com already had an account, and is now in dragons as an officer."},
			wantSlug:   "dragons",
			wantInvite: campaign.InviteRequest{Email: "a@example.com", Role: campaign.RoleOfficer},
		},
		"someone without one": {
			args: []string{"invite", "dragons", "b@example.com", "--username", "bee"},
			campaigns: &fakeCampaigns{invited: &campaign.Invited{Status: campaign.StatusInvited, Invitation: &campaign.InvitationLink{
				URL: "https://auth.example.com/if/flow/campaign-enrollment/?itoken=x", Expires: expires,
			}}},
			wantOut:    []string{"Invited b@example.com into dragons as a mapper", "Send them this link", "?itoken=x"},
			wantSlug:   "dragons",
			wantInvite: campaign.InviteRequest{Email: "b@example.com", Role: campaign.RoleMapper, Username: "bee"},
		},
		"a role that is not one": {
			args:    []string{"invite", "dragons", "a@example.com", "--role", "owner"},
			wantErr: `role "owner" must be mapper or officer`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			if tt.campaigns != nil {
				*e.campaigns = *tt.campaigns
			}
			e.loggedIn(t, time.Now().Add(time.Hour))

			stdout, stderr, err := e.run(t, tt.args...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v\n%s", err, stderr)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(stdout, want) {
					t.Errorf("printed:\n%s\nwant it to contain %q", stdout, want)
				}
			}
			if tt.wantSlug != "" && (e.campaigns.slug != tt.wantSlug || e.campaigns.invite != tt.wantInvite) {
				t.Errorf("invited into %s with %+v, want %s with %+v", e.campaigns.slug, e.campaigns.invite, tt.wantSlug, tt.wantInvite)
			}
		})
	}
}

func TestServerURL(t *testing.T) {
	tests := map[string]struct {
		flag, env, file string
		want, wantErr   string
	}{
		"the flag first":         {flag: "https://flag.example.com", env: "https://env.example.com", file: "https://file.example.com", want: "https://flag.example.com"},
		"then the environment":   {env: "https://env.example.com", file: "https://file.example.com", want: "https://env.example.com"},
		"then the config file":   {file: "https://file.example.com/", want: "https://file.example.com"},
		"none at all":            {wantErr: "no server is set"},
		"not a URL":              {flag: "campaigns.example.com", wantErr: "must be a URL"},
		"http to somewhere else": {flag: "http://campaigns.example.com", wantErr: "must be https"},
		"http on this machine":   {flag: "http://localhost:8080", want: "http://localhost:8080"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := &app{server: tt.flag, getenv: func(k string) string {
				if k == "MPR_SERVER" {
					return tt.env
				}
				return ""
			}}
			got, err := a.serverURL(&Config{Server: tt.file})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("serverURL() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("serverURL() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestConfigFile(t *testing.T) {
	e := newTestEnv(t)

	if _, _, err := e.run(t, "config", "set", "server", "https://campaigns.example.com/"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := e.run(t, "config", "get", "server")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "https://campaigns.example.com\n" {
		t.Errorf("config get server = %q, want the URL without its trailing slash", stdout)
	}
	if _, _, err := e.run(t, "config", "set", "colour", "blue"); err == nil {
		t.Error("config set colour worked, want unknown setting")
	}

	if err := os.WriteFile(e.config, []byte(`{"sever": "https://campaigns.example.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run(t, "config", "get", "server"); err == nil || !strings.Contains(err.Error(), `unknown field "sever"`) {
		t.Errorf("a misspelled setting gave error %v, want unknown field", err)
	}

	e.env["MPR_CONFIG"] = filepath.Join(t.TempDir(), "other.json")
	a := &app{getenv: func(k string) string { return e.env[k] }}
	if got, _ := a.configFile(); got != e.env["MPR_CONFIG"] {
		t.Errorf("configFile() = %q, want MPR_CONFIG's %q", got, e.env["MPR_CONFIG"])
	}
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
