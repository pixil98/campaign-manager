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
		contentType string
		body        string
		wantError   string
	}{
		"the API's own error": {
			status: http.StatusForbidden, contentType: "application/json", body: `{"error": "not allowed: only an admin can create a campaign"}`,
			wantError: "not allowed: only an admin can create a campaign (403 Forbidden)",
		},
		"a proxy's text": {
			status: http.StatusBadGateway, contentType: "text/plain; charset=utf-8", body: "Bad Gateway\n",
			wantError: "Bad Gateway (502 Bad Gateway)",
		},
		"a long text is cut short": {
			status: http.StatusBadGateway, contentType: "text/plain", body: strings.Repeat("x", 500),
			wantError: strings.Repeat("x", maxErrorText) + "... (502 Bad Gateway)",
		},
		"a page is left out": {
			status: http.StatusNotFound, contentType: "text/html", body: "<html>not found</html>",
			wantError: "the server answered 404 Not Found",
		},
		"nothing at all": {
			status:    http.StatusServiceUnavailable,
			wantError: "the server answered 503 Service Unavailable",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := New(srv.URL, srv.Client()).Me(context.Background())
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Status != tt.status {
				t.Fatalf("Me() error = %#v, want an *Error with status %d", err, tt.status)
			}
			if err.Error() != tt.wantError {
				t.Errorf("Me() error = %q, want %q", err.Error(), tt.wantError)
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
