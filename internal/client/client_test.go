package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pixil98/campaign-manager/internal/campaign"
)

func TestErrors(t *testing.T) {
	tests := map[string]struct {
		status      int
		body        string
		wantMessage string
	}{
		"the API's own error": {
			status: http.StatusForbidden, body: `{"error": "not allowed: only an admin can create a campaign"}`,
			wantMessage: "not allowed: only an admin can create a campaign",
		},
		"a proxy's page": {
			status: http.StatusBadGateway, body: "<html>bad gateway</html>",
			wantMessage: "<html>bad gateway</html>",
		},
		"a long page is cut short": {
			status: http.StatusBadGateway, body: strings.Repeat("x", 500),
			wantMessage: strings.Repeat("x", maxErrorText) + "...",
		},
		"nothing at all": {
			status:      http.StatusServiceUnavailable,
			wantMessage: "the server answered 503 Service Unavailable",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := New(srv.URL, srv.Client()).Me(context.Background())
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("Me() error = %v, want an *Error", err)
			}
			if apiErr.Status != tt.status || apiErr.Message != tt.wantMessage {
				t.Errorf("Me() error = %d %q, want %d %q", apiErr.Status, apiErr.Message, tt.status, tt.wantMessage)
			}
		})
	}
}

func TestInviteEscapesTheSlug(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"status": "added"}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL+"/", srv.Client()).Invite(context.Background(), "a/b", campaign.InviteRequest{Email: "a@example.com", Role: campaign.RoleMapper}); err != nil {
		t.Fatal(err)
	}
	if want := "/campaigns/a%2Fb/members"; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
}
