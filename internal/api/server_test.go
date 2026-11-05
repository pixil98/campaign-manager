package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pixil98/campaign-manager/internal/campaign"
)

// fakeAuth accepts only the tokens it holds.
type fakeAuth map[string]campaign.Caller

func (a fakeAuth) Caller(_ context.Context, token string) (campaign.Caller, error) {
	c, ok := a[token]
	if !ok {
		return campaign.Caller{}, errors.New("bad token")
	}
	return c, nil
}

// fakeCampaigns answers with err if set, and records what it was asked.
type fakeCampaigns struct {
	err     error
	created *campaign.Created
	caller  campaign.Caller
	slug    string
	invite  campaign.InviteRequest
}

func (f *fakeCampaigns) Create(_ context.Context, caller campaign.Caller, req campaign.CreateRequest) (*campaign.Created, error) {
	f.caller = caller
	if f.err != nil {
		return nil, f.err
	}
	if f.created != nil {
		return f.created, nil
	}
	return &campaign.Created{Slug: campaign.Slug(req.Slug), New: true}, nil
}

func (f *fakeCampaigns) Invite(_ context.Context, caller campaign.Caller, slug string, req campaign.InviteRequest) (*campaign.Invited, error) {
	f.caller, f.slug, f.invite = caller, slug, req
	if f.err != nil {
		return nil, f.err
	}
	return &campaign.Invited{Status: campaign.StatusAdded}, nil
}

var olive = campaign.Caller{Username: "olive", Roles: map[campaign.Slug][]campaign.Role{"epic": {campaign.RoleOfficer}}}

func serve(t *testing.T, c *fakeCampaigns, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	login := Login{Issuer: "https://auth.example.com/application/o/campaign-manager/", ClientId: "campaign-manager", Scopes: []string{"openid"}}
	srv := NewServer(":0", login, fakeAuth{"good": olive}, c)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRoutes(t *testing.T) {
	tests := map[string]struct {
		method, path, token, body string
		campaigns                 *fakeCampaigns
		wantStatus                int
		wantBody                  string
	}{
		"health needs no token": {
			method: "GET", path: "/healthz",
			wantStatus: http.StatusOK, wantBody: "ok",
		},
		"how to log in needs no token": {
			method: "GET", path: "/auth",
			wantStatus: http.StatusOK, wantBody: `"client_id":"campaign-manager"`,
		},
		"no token": {
			method: "POST", path: "/campaigns", body: `{"slug": "epic"}`,
			wantStatus: http.StatusUnauthorized, wantBody: "bearer token is required",
		},
		"a bad token": {
			method: "POST", path: "/campaigns", token: "bad", body: `{"slug": "epic"}`,
			wantStatus: http.StatusUnauthorized, wantBody: "not valid",
		},
		"who am I": {
			method: "GET", path: "/me", token: "good",
			wantStatus: http.StatusOK, wantBody: `"epic":["officer"]`,
		},
		"a new campaign": {
			method: "POST", path: "/campaigns", token: "good", body: `{"slug": "epic"}`,
			wantStatus: http.StatusCreated, wantBody: `"slug":"epic"`,
		},
		"an existing campaign filled in": {
			method: "POST", path: "/campaigns", token: "good", body: `{"slug": "epic"}`,
			campaigns:  &fakeCampaigns{created: &campaign.Created{Slug: "epic"}},
			wantStatus: http.StatusOK, wantBody: `"new":false`,
		},
		"an invite": {
			method: "POST", path: "/campaigns/epic/members", token: "good", body: `{"email": "a@example.com", "role": "mapper"}`,
			wantStatus: http.StatusOK, wantBody: `"status":"added"`,
		},
		"a misspelled field": {
			method: "POST", path: "/campaigns/epic/members", token: "good", body: `{"emial": "a@example.com"}`,
			wantStatus: http.StatusBadRequest, wantBody: "unknown field",
		},
		"not JSON": {
			method: "POST", path: "/campaigns", token: "good", body: `slug=epic`,
			wantStatus: http.StatusBadRequest, wantBody: "reading request body",
		},
		"the wrong method": {
			method: "GET", path: "/campaigns", token: "good",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := tt.campaigns
			if c == nil {
				c = &fakeCampaigns{}
			}
			rec := serve(t, c, tt.method, tt.path, tt.token, tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("%s %s = %d %s, want %d", tt.method, tt.path, rec.Code, rec.Body, tt.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("%s %s body = %s, want it to contain %s", tt.method, tt.path, rec.Body, tt.wantBody)
			}
		})
	}
}

// TestInvitePassesThrough checks the caller, the slug from the path and the
// request reach the campaign manager.
func TestInvitePassesThrough(t *testing.T) {
	c := &fakeCampaigns{}
	serve(t, c, "POST", "/campaigns/epic/members", "good", `{"email": "a@example.com", "role": "officer", "username": "a"}`)
	if c.slug != "epic" || c.caller.Username != "olive" {
		t.Errorf("manager got slug %q, caller %q; want epic, olive", c.slug, c.caller.Username)
	}
	if want := (campaign.InviteRequest{Email: "a@example.com", Role: campaign.RoleOfficer, Username: "a"}); c.invite != want {
		t.Errorf("manager got %+v, want %+v", c.invite, want)
	}
}

// TestErrorStatuses checks each of the manager's errors becomes its status,
// and that anything else is a 500 that shows none of its details.
func TestErrorStatuses(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantStatus int
		wantBody   string
		wantNot    string
	}{
		"invalid":   {err: fmt.Errorf("%w: bad slug", campaign.ErrInvalid), wantStatus: http.StatusBadRequest, wantBody: "bad slug"},
		"forbidden": {err: fmt.Errorf("%w: not yours", campaign.ErrForbidden), wantStatus: http.StatusForbidden, wantBody: "not yours"},
		"not found": {err: fmt.Errorf("%w: epic", campaign.ErrNotFound), wantStatus: http.StatusNotFound, wantBody: "no such campaign"},
		"conflict":  {err: fmt.Errorf("%w: already in", campaign.ErrConflict), wantStatus: http.StatusConflict, wantBody: "already in"},
		"anything else": {
			err:        errors.New("authentik: POST /api/v3/core/groups/: 500 internal detail"),
			wantStatus: http.StatusInternalServerError,
			wantBody:   "see the service's logs",
			wantNot:    "internal detail",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec := serve(t, &fakeCampaigns{err: tt.err}, "POST", "/campaigns/epic/members", "good", `{"email": "a@example.com", "role": "mapper"}`)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			var body errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %s is not an error: %v", rec.Body, err)
			}
			if !strings.Contains(body.Error, tt.wantBody) {
				t.Errorf("error = %q, want it to mention %q", body.Error, tt.wantBody)
			}
			if tt.wantNot != "" && strings.Contains(body.Error, tt.wantNot) {
				t.Errorf("error = %q, should not show %q", body.Error, tt.wantNot)
			}
		})
	}
}
