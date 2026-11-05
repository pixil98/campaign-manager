package authentik

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// request is what the fake authentik was sent.
type request struct {
	method, path, query, auth string
	body                      map[string]any
}

// newTestClient returns a client for a fake authentik that answers each
// "METHOD /path?query" from replies, and records every request.
func newTestClient(t *testing.T, replies map[string]string) (*Client, *[]request) {
	t.Helper()
	var got []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := request{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			if err := json.Unmarshal(data, &req.body); err != nil {
				t.Errorf("request body %q is not JSON: %v", data, err)
			}
		}
		got = append(got, req)
		key := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		reply, ok := replies[key]
		if !ok {
			http.Error(w, `{"detail": "Not found."}`, http.StatusNotFound)
			return
		}
		if reply == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "secret-token", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c, &got
}

func TestGroupPagesAndMatchesExactly(t *testing.T) {
	c, got := newTestClient(t, map[string]string{
		"GET /api/v3/core/groups/?include_users=true&name=mapper-epic-mapper&page=1": `{
			"pagination": {"next": 2},
			"results": [{"pk": "a", "name": "mapper-epic-mappers"}]}`,
		"GET /api/v3/core/groups/?include_users=true&name=mapper-epic-mapper&page=2": `{
			"pagination": {"next": 0},
			"results": [{"pk": "b", "name": "mapper-epic-mapper", "users": [3, 4], "attributes": {"campaign": "epic"}}]}`,
	})

	g, err := c.Group(context.Background(), "mapper-epic-mapper")
	if err != nil {
		t.Fatalf("Group = %v", err)
	}
	want := &Group{Pk: "b", Name: "mapper-epic-mapper", Users: []int{3, 4}, Attributes: map[string]any{"campaign": "epic"}}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("Group = %+v, want %+v", g, want)
	}
	if len(*got) != 2 || (*got)[0].auth != "Bearer secret-token" {
		t.Errorf("requests = %+v, want two pages with the token", *got)
	}
}

func TestGroupMissing(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{
		"GET /api/v3/core/groups/?include_users=true&name=nope&page=1": `{"pagination": {"next": 0}, "results": []}`,
	})
	g, err := c.Group(context.Background(), "nope")
	if g != nil || err != nil {
		t.Errorf("Group(nope) = %v, %v; want nil, nil", g, err)
	}
}

// TestUsersByEmailIgnoresCase checks an account stored with capitals is
// found, and a search result that only contains the address is not.
func TestUsersByEmailIgnoresCase(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{
		"GET /api/v3/core/users/?page=1&search=alice%40example.com": `{"pagination": {"next": 0}, "results": [
			{"pk": 1, "username": "alice", "email": "Alice@Example.com"},
			{"pk": 2, "username": "malice", "email": "malice@example.com"}]}`,
	})
	users, err := c.UsersByEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("UsersByEmail = %v", err)
	}
	if want := []User{{Pk: 1, Username: "alice", Email: "Alice@Example.com"}}; !reflect.DeepEqual(users, want) {
		t.Errorf("UsersByEmail = %+v, want %+v", users, want)
	}
}

func TestWrites(t *testing.T) {
	expires := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		call     func(*Client) error
		reply    string
		wantReq  request
		wantBody map[string]any
	}{
		"create a group": {
			call: func(c *Client) error {
				_, err := c.CreateGroup(context.Background(), "mapper-epic-officer", "base-pk", map[string]any{"campaign": "epic"})
				return err
			},
			reply:   `{"pk": "new", "name": "mapper-epic-officer"}`,
			wantReq: request{method: "POST", path: "/api/v3/core/groups/"},
			wantBody: map[string]any{
				"name": "mapper-epic-officer", "parents": []any{"base-pk"}, "attributes": map[string]any{"campaign": "epic"},
			},
		},
		"add a user to a group": {
			call:     func(c *Client) error { return c.AddUserToGroup(context.Background(), "group-pk", 7) },
			wantReq:  request{method: "POST", path: "/api/v3/core/groups/group-pk/add_user/"},
			wantBody: map[string]any{"pk": float64(7)},
		},
		"create an invitation": {
			call: func(c *Client) error {
				_, err := c.CreateInvitation(context.Background(), Invitation{
					Name: "campaign-epic-x", Expires: expires, SingleUse: true, Flow: "flow-pk",
					FixedData: InvitationData{Email: "bob@example.com", Groups: []string{"mapper-epic-mapper"}},
				})
				return err
			},
			reply:   `{"pk": "inv", "name": "campaign-epic-x"}`,
			wantReq: request{method: "POST", path: "/api/v3/stages/invitation/invitations/"},
			wantBody: map[string]any{
				"name": "campaign-epic-x", "expires": "2026-11-12T00:00:00Z", "single_use": true, "flow": "flow-pk",
				"fixed_data": map[string]any{"email": "bob@example.com", "groups": []any{"mapper-epic-mapper"}},
			},
		},
		"email an invitation": {
			call:     func(c *Client) error { return c.SendInvitation(context.Background(), "inv", "bob@example.com") },
			wantReq:  request{method: "POST", path: "/api/v3/stages/invitation/invitations/inv/send_email/"},
			wantBody: map[string]any{"email_addresses": []any{"bob@example.com"}},
		},
		"delete an invitation": {
			call:    func(c *Client) error { return c.DeleteInvitation(context.Background(), "inv") },
			wantReq: request{method: "DELETE", path: "/api/v3/stages/invitation/invitations/inv/"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c, got := newTestClient(t, map[string]string{tt.wantReq.method + " " + tt.wantReq.path: tt.reply})
			if err := tt.call(c); err != nil {
				t.Fatalf("call = %v", err)
			}
			if len(*got) != 1 {
				t.Fatalf("requests = %+v, want one", *got)
			}
			req := (*got)[0]
			if req.method != tt.wantReq.method || req.path != tt.wantReq.path {
				t.Errorf("sent %s %s, want %s %s", req.method, req.path, tt.wantReq.method, tt.wantReq.path)
			}
			if !reflect.DeepEqual(req.body, tt.wantBody) {
				t.Errorf("body = %v, want %v", req.body, tt.wantBody)
			}
		})
	}
}

func TestFlowPk(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{
		"GET /api/v3/flows/instances/campaign-enrollment/": `{"pk": "flow-pk", "slug": "campaign-enrollment"}`,
	})
	pk, err := c.FlowPk(context.Background(), "campaign-enrollment")
	if err != nil || pk != "flow-pk" {
		t.Errorf("FlowPk = %q, %v; want flow-pk", pk, err)
	}
}

func TestAPIError(t *testing.T) {
	c, _ := newTestClient(t, nil)
	_, err := c.FlowPk(context.Background(), "nope")
	if !IsNotFound(err) {
		t.Fatalf("FlowPk(nope) = %v, want a not found error", err)
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "Not found.") {
		t.Errorf("error = %q, want the status and authentik's message", err)
	}
}
