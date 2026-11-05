package dokuwiki

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// call is a JSON-RPC call the fake wiki was sent.
type call struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// newTestClient returns a client for a fake wiki that answers each method
// with reply, a status and a JSON body, and records every call.
func newTestClient(t *testing.T, replies map[string]reply) (*Client, *[]call) {
	t.Helper()
	var got []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lib/exe/jsonrpc.php" || r.Method != http.MethodPost {
			t.Errorf("sent %s %s, want POST /lib/exe/jsonrpc.php", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer wiki-token" {
			t.Errorf("Authorization = %q, want the bearer token", auth)
		}
		var body struct {
			call
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding call: %v", err)
		}
		if body.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q, want 2.0", body.JSONRPC)
		}
		got = append(got, body.call)
		rep := replies[body.Method]
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "wiki-token", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c, &got
}

type reply struct {
	status int
	body   string
}

func ok(result string) reply {
	return reply{http.StatusOK, `{"jsonrpc": "2.0", "id": 1, "result": ` + result + `}`}
}

// TestCalls pins each method's name and its named parameters, which DokuWiki
// matches against its PHP parameter names.
func TestCalls(t *testing.T) {
	c, got := newTestClient(t, map[string]reply{
		"plugin.acl.addAcl": ok(`true`),
		"core.listPages":    ok(`[{"id": "templates:notes"}, {"id": "templates:lists:people"}]`),
		"core.getPage":      ok(`"some text"`),
		"core.savePage":     ok(`true`),
	})
	ctx := context.Background()

	if err := c.AddACL(ctx, "epic:*", "@mapper-epic-mapper", 1); err != nil {
		t.Errorf("AddACL = %v", err)
	}
	pages, err := c.ListPages(ctx, "templates")
	if err != nil || !reflect.DeepEqual(pages, []string{"templates:notes", "templates:lists:people"}) {
		t.Errorf("ListPages = %v, %v", pages, err)
	}
	text, err := c.Page(ctx, "templates:notes")
	if err != nil || text != "some text" {
		t.Errorf("Page = %q, %v", text, err)
	}
	if err := c.SavePage(ctx, "epic:notes", "some text", "Copied"); err != nil {
		t.Errorf("SavePage = %v", err)
	}

	want := []call{
		{Method: "plugin.acl.addAcl", Params: map[string]any{"scope": "epic:*", "user": "@mapper-epic-mapper", "level": float64(1)}},
		{Method: "core.listPages", Params: map[string]any{"namespace": "templates", "depth": float64(0)}},
		{Method: "core.getPage", Params: map[string]any{"page": "templates:notes"}},
		{Method: "core.savePage", Params: map[string]any{"page": "epic:notes", "text": "some text", "summary": "Copied"}},
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("calls = %+v\nwant %+v", *got, want)
	}
}

func TestErrors(t *testing.T) {
	tests := map[string]struct {
		reply    reply
		wantCode int
		wantMsg  string
	}{
		"an error result": {
			reply:    reply{http.StatusForbidden, `{"jsonrpc": "2.0", "id": 1, "error": {"code": -32604, "message": "server error. forbidden to call the method plugin.acl.addAcl"}}`},
			wantCode: -32604,
		},
		"a false result": {
			reply:   ok(`false`),
			wantMsg: "dokuwiki: plugin.acl.addAcl: rule for @g on ns:* was not saved",
		},
		"not JSON at all": {
			reply:   reply{http.StatusBadGateway, `<html>bad gateway</html>`},
			wantMsg: "dokuwiki: plugin.acl.addAcl: 502 <html>bad gateway</html>",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"plugin.acl.addAcl": tt.reply})
			err := c.AddACL(context.Background(), "ns:*", "@g", 1)
			if err == nil {
				t.Fatal("AddACL = nil, want an error")
			}
			var rpcErr *RPCError
			if tt.wantCode != 0 {
				if !errors.As(err, &rpcErr) || rpcErr.Code != tt.wantCode {
					t.Errorf("AddACL = %v, want an RPC error with code %d", err, tt.wantCode)
				}
				return
			}
			if err.Error() != tt.wantMsg {
				t.Errorf("AddACL = %q, want %q", err, tt.wantMsg)
			}
		})
	}
}
