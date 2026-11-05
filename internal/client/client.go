// Package client calls campaign-manager's API, with the API's own request and
// answer types, so the two cannot drift apart.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/pixil98/campaign-manager/internal/api"
	"github.com/pixil98/campaign-manager/internal/campaign"
)

// Client calls one campaign-manager server. Its HTTP client is what adds the
// caller's token; Login needs none.
type Client struct {
	server string
	hc     *http.Client
}

// New returns a client for the server at server, such as
// https://campaigns.example.com, sending requests with hc.
func New(server string, hc *http.Client) *Client {
	return &Client{server: strings.TrimSuffix(server, "/"), hc: hc}
}

// Error is an answer from the API other than success. Message is what the
// server said, if anything worth showing.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the server answered %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s (%d %s)", e.Message, e.Status, http.StatusText(e.Status))
}

// Login returns how to get a token the server takes.
func (c *Client) Login(ctx context.Context) (*api.Login, error) {
	var l api.Login
	if err := c.do(ctx, http.MethodGet, "/auth", nil, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// Me returns the caller, as the server reads them from their token.
func (c *Client) Me(ctx context.Context) (*campaign.Caller, error) {
	var me campaign.Caller
	if err := c.do(ctx, http.MethodGet, "/me", nil, &me); err != nil {
		return nil, err
	}
	return &me, nil
}

// CreateCampaign creates a campaign, or fills in what an existing one is
// missing.
func (c *Client) CreateCampaign(ctx context.Context, req campaign.CreateRequest) (*campaign.Created, error) {
	var created campaign.Created
	if err := c.do(ctx, http.MethodPost, "/campaigns", req, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

// Invite adds a person to campaign slug, or invites them if they have no
// account.
func (c *Client) Invite(ctx context.Context, slug string, req campaign.InviteRequest) (*campaign.Invited, error) {
	var invited campaign.Invited
	if err := c.do(ctx, http.MethodPost, "/campaigns/"+url.PathEscape(slug)+"/members", req, &invited); err != nil {
		return nil, err
	}
	return &invited, nil
}

// do sends body as JSON, if there is one, and decodes the answer into out.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the answer to %s %s: %w", method, path, err)
	}
	return nil
}

// maxErrorText bounds how much of an answer that is not the API's own, such
// as a proxy's error text, ends up in an error.
const maxErrorText = 200

// readError reads the API's {"error": "..."} answer, or failing that, the
// start of a plain text answer. Anything else, such as an HTML error page,
// is no use in a terminal, so only the status is kept.
func readError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error != "" {
		return &Error{Status: resp.StatusCode, Message: body.Error}
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/plain" {
		return &Error{Status: resp.StatusCode}
	}
	text := strings.TrimSpace(string(data))
	if len(text) > maxErrorText {
		text = text[:maxErrorText] + "..."
	}
	return &Error{Status: resp.StatusCode, Message: text}
}
