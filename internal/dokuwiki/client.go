// Package dokuwiki is a client for the parts of DokuWiki's JSON-RPC API that
// campaigns need: ACL rules and pages.
package dokuwiki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// RPCError is an error DokuWiki's API returned.
type RPCError struct {
	Method  string
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("dokuwiki: %s: %s (code %d)", e.Method, e.Message, e.Code)
}

// Client calls DokuWiki's JSON-RPC API with a bearer token. DokuWiki checks
// the token before any other login, so the account it names needs no
// password, only an entry in the wiki's user file.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
	ids      atomic.Int64
}

// New returns a client for the wiki at baseURL.
func New(baseURL, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing wiki url: %w", err)
	}
	return &Client{endpoint: u.JoinPath("lib/exe/jsonrpc.php").String(), token: token, http: hc}, nil
}

// AddACL sets subject's level on scope, adding the rule or updating one
// already there. Group subjects start with @; DokuWiki encodes their names
// itself. Only a wiki superuser may call it.
func (c *Client) AddACL(ctx context.Context, scope, subject string, level int) error {
	var ok bool
	if err := c.call(ctx, "plugin.acl.addAcl", map[string]any{"scope": scope, "user": subject, "level": level}, &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("dokuwiki: plugin.acl.addAcl: rule for %s on %s was not saved", subject, scope)
	}
	return nil
}

// ListPages returns the id of every page in namespace and below it.
func (c *Client) ListPages(ctx context.Context, namespace string) ([]string, error) {
	var pages []struct {
		Id string `json:"id"`
	}
	// Depth 0 is every level below the namespace.
	if err := c.call(ctx, "core.listPages", map[string]any{"namespace": namespace, "depth": 0}, &pages); err != nil {
		return nil, err
	}
	ids := make([]string, len(pages))
	for i, p := range pages {
		ids[i] = p.Id
	}
	return ids, nil
}

// Page returns the wiki text of page id.
func (c *Client) Page(ctx context.Context, id string) (string, error) {
	var text string
	if err := c.call(ctx, "core.getPage", map[string]any{"page": id}, &text); err != nil {
		return "", err
	}
	return text, nil
}

// SavePage writes text to page id, creating it if needed.
func (c *Client) SavePage(ctx context.Context, id, text, summary string) error {
	var ok bool
	if err := c.call(ctx, "core.savePage", map[string]any{"page": id, "text": text, "summary": summary}, &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("dokuwiki: core.savePage: %s was not saved", id)
	}
	return nil
}

// call makes one JSON-RPC 2.0 call. Parameters are named, which DokuWiki
// maps onto its methods' PHP parameter names.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.ids.Add(1),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("dokuwiki: encoding %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("dokuwiki: %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dokuwiki: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// DokuWiki answers errors with a 4xx status and a JSON-RPC error body,
	// so the body is read whatever the status.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("dokuwiki: reading %s: %w", method, err)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("dokuwiki: %s: %d %s", method, resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 512)])))
	}
	if out.Error != nil {
		out.Error.Method = method
		return out.Error
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dokuwiki: %s: unexpected status %d", method, resp.StatusCode)
	}
	if err := json.Unmarshal(out.Result, result); err != nil {
		return fmt.Errorf("dokuwiki: decoding %s result: %w", method, err)
	}
	return nil
}
