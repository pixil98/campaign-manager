// Package authentik is a client for the parts of authentik's API that
// campaigns need: groups, users and invitations.
package authentik

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Group is an authentik group. Users holds the pks of its direct members.
type Group struct {
	Pk         string         `json:"pk"`
	Name       string         `json:"name"`
	Users      []int          `json:"users"`
	Attributes map[string]any `json:"attributes"`
}

// User is an authentik user.
type User struct {
	Pk       int    `json:"pk"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

// Invitation is an invitation to enroll through a flow. FixedData is what the
// enrollment flow writes into the new account.
type Invitation struct {
	Pk        string         `json:"pk,omitempty"`
	Name      string         `json:"name"`
	Expires   time.Time      `json:"expires"`
	SingleUse bool           `json:"single_use"`
	Flow      string         `json:"flow"`
	FixedData InvitationData `json:"fixed_data"`
}

// InvitationData is an invitation's fixed_data, as the enrollment flow reads
// it: the account's email, and the names of the groups it joins. Prefill
// holds suggestions the sign-up form starts with.
type InvitationData struct {
	Email   string            `json:"email"`
	Groups  []string          `json:"groups"`
	Prefill map[string]string `json:"prefill,omitempty"`
}

// APIError is a response from authentik other than success.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("authentik: %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// Client calls authentik's API with an API token.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New returns a client for the authentik at baseURL. It has to be authentik's
// public address, since authentik builds the links in invitation emails from
// the host it is called on.
func New(baseURL, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing authentik url: %w", err)
	}
	return &Client{base: u, token: token, http: hc}, nil
}

// Group returns the group called name, or nil if there is none.
func (c *Client) Group(ctx context.Context, name string) (*Group, error) {
	groups, err := list[Group](ctx, c, "/api/v3/core/groups/", url.Values{"name": {name}, "include_users": {"true"}})
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.Name == name {
			return &g, nil
		}
	}
	return nil, nil
}

// CreateGroup makes a group under parent, the pk of an existing group.
func (c *Client) CreateGroup(ctx context.Context, name, parent string, attributes map[string]any) (*Group, error) {
	body := map[string]any{"name": name, "parents": []string{parent}, "attributes": attributes}
	var g Group
	if err := c.do(ctx, http.MethodPost, "/api/v3/core/groups/", nil, body, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// UsersByEmail returns every user whose email is email, ignoring case.
// authentik's email filter is exact and case-sensitive, so this searches
// and compares instead, or an account stored as Alice@ would be missed.
func (c *Client) UsersByEmail(ctx context.Context, email string) ([]User, error) {
	users, err := list[User](ctx, c, "/api/v3/core/users/", url.Values{"search": {email}})
	if err != nil {
		return nil, err
	}
	var matched []User
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			matched = append(matched, u)
		}
	}
	return matched, nil
}

// AddUserToGroup makes user a direct member of group.
func (c *Client) AddUserToGroup(ctx context.Context, group string, user int) error {
	path := "/api/v3/core/groups/" + url.PathEscape(group) + "/add_user/"
	return c.do(ctx, http.MethodPost, path, nil, map[string]int{"pk": user}, nil)
}

// Invitation returns the invitation called name, expired or not, or nil if
// there is none.
func (c *Client) Invitation(ctx context.Context, name string) (*Invitation, error) {
	invs, err := list[Invitation](ctx, c, "/api/v3/stages/invitation/invitations/", url.Values{"name": {name}})
	if err != nil {
		return nil, err
	}
	for _, inv := range invs {
		if inv.Name == name {
			return &inv, nil
		}
	}
	return nil, nil
}

// Invitations returns every invitation, expired ones included.
func (c *Client) Invitations(ctx context.Context) ([]Invitation, error) {
	return list[Invitation](ctx, c, "/api/v3/stages/invitation/invitations/", nil)
}

// CreateInvitation makes inv and returns it as stored.
func (c *Client) CreateInvitation(ctx context.Context, inv Invitation) (*Invitation, error) {
	var created Invitation
	if err := c.do(ctx, http.MethodPost, "/api/v3/stages/invitation/invitations/", nil, inv, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

// DeleteInvitation removes the invitation with pk.
func (c *Client) DeleteInvitation(ctx context.Context, pk string) error {
	return c.do(ctx, http.MethodDelete, "/api/v3/stages/invitation/invitations/"+url.PathEscape(pk)+"/", nil, nil, nil)
}

// SendInvitation has authentik email the invitation with pk to email.
func (c *Client) SendInvitation(ctx context.Context, pk, email string) error {
	path := "/api/v3/stages/invitation/invitations/" + url.PathEscape(pk) + "/send_email/"
	return c.do(ctx, http.MethodPost, path, nil, map[string][]string{"email_addresses": {email}}, nil)
}

// FlowPk returns the pk of the flow with slug.
func (c *Client) FlowPk(ctx context.Context, slug string) (string, error) {
	var flow struct {
		Pk string `json:"pk"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v3/flows/instances/"+url.PathEscape(slug)+"/", nil, nil, &flow); err != nil {
		return "", err
	}
	return flow.Pk, nil
}

// page is one page of a list response.
type page[T any] struct {
	Pagination struct {
		Next int `json:"next"`
	} `json:"pagination"`
	Results []T `json:"results"`
}

// list gets every page of a list endpoint.
func list[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	var all []T
	for n := 1; n > 0; {
		q.Set("page", strconv.Itoa(n))
		var p page[T]
		if err := c.do(ctx, http.MethodGet, path, q, nil, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Results...)
		n = p.Pagination.Next
	}
	return all, nil
}

// do sends one request, and decodes the response into out unless out is nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.base.JoinPath(path)
	// JoinPath drops the trailing slash authentik's routes need.
	if strings.HasSuffix(path, "/") && !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.RawQuery = query.Encode()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("authentik: encoding %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return fmt.Errorf("authentik: %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("authentik: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("authentik: decoding %s %s: %w", method, path, err)
	}
	return nil
}

// IsNotFound reports whether err is authentik answering 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}
