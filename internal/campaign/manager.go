package campaign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/pixil98/campaign-manager/internal/authentik"
)

// Directory is the part of authentik the manager uses. Lookups return nil,
// not an error, when there is nothing to find.
type Directory interface {
	Group(ctx context.Context, name string) (*authentik.Group, error)
	CreateGroup(ctx context.Context, name, parent string, attributes map[string]any) (*authentik.Group, error)
	UsersByEmail(ctx context.Context, email string) ([]authentik.User, error)
	AddUserToGroup(ctx context.Context, group string, user int) error
	Invitation(ctx context.Context, name string) (*authentik.Invitation, error)
	Invitations(ctx context.Context) ([]authentik.Invitation, error)
	CreateInvitation(ctx context.Context, inv authentik.Invitation) (*authentik.Invitation, error)
	DeleteInvitation(ctx context.Context, pk string) error
	SendInvitation(ctx context.Context, pk, email string) error
	FlowPk(ctx context.Context, slug string) (string, error)
}

// Wiki is the part of DokuWiki the manager uses.
type Wiki interface {
	AddACL(ctx context.Context, scope, subject string, level int) error
	ListPages(ctx context.Context, namespace string) ([]string, error)
	Page(ctx context.Context, id string) (string, error)
	SavePage(ctx context.Context, id, text, summary string) error
}

// Options are the manager's settings.
type Options struct {
	// BaseGroup is the group every campaign group sits under, and the first
	// part of their names: mapper-<slug>-<role> for "mapper".
	BaseGroup string
	// AuthentikURL is authentik's public address, which invitation links
	// point at.
	AuthentikURL string
	// InvitationFlow is the slug of the enrollment flow invitations use.
	InvitationFlow string
	// InvitationLifetime is how long an invitation can be redeemed for.
	InvitationLifetime time.Duration
	// SendInvitationEmail has authentik email invitations. Off, the link is
	// only returned, for the inviter to pass on.
	SendInvitationEmail bool
	// TemplatesNamespace is the wiki namespace new campaigns' working pages
	// are copied from.
	TemplatesNamespace string
}

// Manager creates campaigns and invites people into them.
type Manager struct {
	dir  Directory
	wiki Wiki
	opts Options
	now  func() time.Time
}

// NewManager returns a manager working through dir and wiki.
func NewManager(dir Directory, wiki Wiki, opts Options) *Manager {
	return &Manager{dir: dir, wiki: wiki, opts: opts, now: time.Now}
}

// groupName is the authentik group for role in campaign slug.
func (m *Manager) groupName(slug Slug, role Role) string {
	return m.opts.BaseGroup + "-" + string(slug) + "-" + string(role)
}

// CreateRequest asks for a new campaign. Name is the campaign's real name,
// which is kept on its groups and never in the slug.
type CreateRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name,omitempty"`
}

// Created describes a campaign after Create.
type Created struct {
	Slug   Slug            `json:"slug"`
	Groups map[Role]string `json:"groups"`
	// SeededPages are the working pages copied in from the templates this
	// time. Pages already in the campaign are left alone.
	SeededPages []string `json:"seeded_pages"`
	// New is false when the campaign's groups already existed, so only
	// what was missing was added.
	New bool `json:"new"`
}

// Create makes campaign req.Slug: its two groups, its two wiki rules, and its
// working pages. Every step finds before it makes, so a failed run can simply
// be repeated. Only an admin may create a campaign.
func (m *Manager) Create(ctx context.Context, caller Caller, req CreateRequest) (*Created, error) {
	if !caller.Admin {
		return nil, fmt.Errorf("%w: only an admin can create a campaign", ErrForbidden)
	}
	slug, err := ParseSlug(req.Slug)
	if err != nil {
		return nil, err
	}

	base, err := m.dir.Group(ctx, m.opts.BaseGroup)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, fmt.Errorf("base group %q not found in authentik", m.opts.BaseGroup)
	}

	created := &Created{Slug: slug, Groups: map[Role]string{}}
	for _, role := range Roles {
		name := m.groupName(slug, role)
		g, err := m.dir.Group(ctx, name)
		if err != nil {
			return nil, err
		}
		if g != nil {
			if err := checkGroup(g, slug, role); err != nil {
				return nil, err
			}
		} else {
			attrs := map[string]any{"campaign": string(slug), "role": string(role)}
			if req.Name != "" {
				attrs["display_name"] = req.Name
			}
			if _, err := m.dir.CreateGroup(ctx, name, base.Pk, attrs); err != nil {
				return nil, fmt.Errorf("creating group %s: %w", name, err)
			}
			created.New = true
		}
		created.Groups[role] = name
	}

	// The rules come before anyone is invited, so a first login already
	// works. DokuWiki rules are just names, so the order against the
	// groups does not matter to it.
	for _, role := range Roles {
		if err := m.wiki.AddACL(ctx, string(slug)+":*", "@"+m.groupName(slug, role), role.wikiLevel()); err != nil {
			return nil, fmt.Errorf("adding wiki rule for %s: %w", role, err)
		}
	}

	created.SeededPages, err = m.seed(ctx, slug)
	if err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "created campaign", "campaign", slug, "by", caller.Username, "new", created.New, "seeded_pages", len(created.SeededPages))
	return created, nil
}

// checkGroup makes sure an existing group is the one this campaign would have
// made, and not something else that happens to have its name.
func checkGroup(g *authentik.Group, slug Slug, role Role) error {
	if g.Attributes["campaign"] != string(slug) || g.Attributes["role"] != string(role) {
		return fmt.Errorf("%w: group %s already exists and is not campaign %s's %s group", ErrConflict, g.Name, slug, role)
	}
	return nil
}

// seed copies every page under the templates namespace into the campaign,
// skipping pages it already has, so a repeated run never overwrites edits.
// The handbook is never copied: every campaign reads the one shared copy.
func (m *Manager) seed(ctx context.Context, slug Slug) ([]string, error) {
	templates, err := m.wiki.ListPages(ctx, m.opts.TemplatesNamespace)
	if err != nil {
		return nil, fmt.Errorf("listing templates: %w", err)
	}
	existing, err := m.wiki.ListPages(ctx, string(slug))
	if err != nil {
		return nil, fmt.Errorf("listing campaign pages: %w", err)
	}

	prefix := m.opts.TemplatesNamespace + ":"
	seeded := []string{}
	for _, id := range templates {
		rest, ok := strings.CutPrefix(id, prefix)
		if !ok {
			continue
		}
		target := string(slug) + ":" + rest
		if slices.Contains(existing, target) {
			continue
		}
		text, err := m.wiki.Page(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("reading template %s: %w", id, err)
		}
		if err := m.wiki.SavePage(ctx, target, text, "Copied from "+id); err != nil {
			return nil, fmt.Errorf("copying %s to %s: %w", id, target, err)
		}
		seeded = append(seeded, target)
	}
	return seeded, nil
}

// InviteRequest asks for a person to join a campaign in a role. Username and
// Name are suggestions a new account's sign-up form starts with.
type InviteRequest struct {
	Email    string `json:"email"`
	Role     Role   `json:"role"`
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Invite statuses.
const (
	// StatusAdded is a person who already had an account, now in the
	// campaign's group.
	StatusAdded = "added"
	// StatusInvited is a person with no account yet, sent an invitation.
	StatusInvited = "invited"
)

// Invited describes what Invite did.
type Invited struct {
	Status     string          `json:"status"`
	Invitation *InvitationLink `json:"invitation,omitempty"`
}

// InvitationLink is how an invitee redeems their invitation.
type InvitationLink struct {
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`
	// Emailed is whether authentik was asked to email the link. When not,
	// the inviter has to pass it on.
	Emailed bool `json:"emailed"`
}

// Invite puts a person into campaign slugText in req.Role. Someone with an
// account joins the campaign's group at once, leaving any other campaigns
// they are in as they were. Someone without one gets an invitation of their
// own for this campaign. An admin may invite into any campaign, and an
// officer only into a campaign they are an officer of.
//
// A person has one role per campaign, so inviting someone already in the
// campaign, or already invited to it in another role, is refused rather than
// changing their role.
func (m *Manager) Invite(ctx context.Context, caller Caller, slugText string, req InviteRequest) (*Invited, error) {
	slug := Slug(slugText)
	// Checked before anything is looked up, so asking about a campaign you
	// are not in reveals nothing about whether it exists.
	if !caller.canInvite(slug) {
		return nil, fmt.Errorf("%w: inviting into %s needs an officer of it or an admin", ErrForbidden, slug)
	}
	if !req.Role.Valid() {
		return nil, fmt.Errorf("%w: role %q must be %q or %q", ErrInvalid, req.Role, RoleMapper, RoleOfficer)
	}
	email, err := parseEmail(req.Email)
	if err != nil {
		return nil, err
	}

	groups, err := m.campaignGroups(ctx, slug)
	if err != nil {
		return nil, err
	}

	users, err := m.dir.UsersByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	switch len(users) {
	case 0:
		return m.invite(ctx, caller, slug, email, req, groups)
	case 1:
		return m.add(ctx, caller, slug, users[0], req.Role, groups)
	default:
		return nil, fmt.Errorf("%w: %d accounts use that email; sort them out in authentik first", ErrConflict, len(users))
	}
}

// add puts an existing account into the campaign.
func (m *Manager) add(ctx context.Context, caller Caller, slug Slug, user authentik.User, role Role, groups map[Role]*authentik.Group) (*Invited, error) {
	for _, r := range Roles {
		if slices.Contains(groups[r].Users, user.Pk) {
			return nil, fmt.Errorf("%w: already in the campaign as %s", ErrConflict, r)
		}
	}
	if err := m.dir.AddUserToGroup(ctx, groups[role].Pk, user.Pk); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "added to campaign", "campaign", slug, "role", role, "user", user.Pk, "by", caller.Username)
	return &Invited{Status: StatusAdded}, nil
}

// invite makes, or reuses, the person's invitation for this campaign. Each
// campaign invites separately, never sharing one invitation between
// campaigns: an inviter is handed the link, and a link that also led into
// another campaign would let them into it. Once the person has an account,
// Reconcile settles their other campaigns' invitations.
func (m *Manager) invite(ctx context.Context, caller Caller, slug Slug, email string, req InviteRequest, groups map[Role]*authentik.Group) (*Invited, error) {
	name := m.invitationName(slug, email)
	inv, err := m.dir.Invitation(ctx, name)
	if err != nil {
		return nil, err
	}
	if inv != nil && !inv.Expires.After(m.now()) {
		if err := m.dir.DeleteInvitation(ctx, inv.Pk); err != nil {
			return nil, fmt.Errorf("removing expired invitation: %w", err)
		}
		inv = nil
	}

	if inv != nil {
		if pending := m.invitedRole(inv, slug); pending != req.Role {
			return nil, fmt.Errorf("%w: already invited to the campaign as %s", ErrConflict, pending)
		}
	} else {
		flow, err := m.dir.FlowPk(ctx, m.opts.InvitationFlow)
		if err != nil {
			return nil, fmt.Errorf("finding flow %s: %w", m.opts.InvitationFlow, err)
		}
		data := authentik.InvitationData{Email: email, Groups: []string{groups[req.Role].Name}}
		if prefill := prefill(req); len(prefill) > 0 {
			data.Prefill = prefill
		}
		inv, err = m.dir.CreateInvitation(ctx, authentik.Invitation{
			Name:      name,
			Expires:   m.now().Add(m.opts.InvitationLifetime),
			SingleUse: true,
			Flow:      flow,
			FixedData: data,
		})
		if err != nil {
			return nil, fmt.Errorf("creating invitation: %w", err)
		}
	}

	link := &InvitationLink{URL: m.invitationURL(inv.Pk), Expires: inv.Expires}
	if m.opts.SendInvitationEmail {
		if err := m.dir.SendInvitation(ctx, inv.Pk, email); err != nil {
			return nil, fmt.Errorf("emailing invitation: %w", err)
		}
		link.Emailed = true
	}
	slog.InfoContext(ctx, "invited to campaign", "campaign", slug, "role", req.Role, "invitation", inv.Name, "emailed", link.Emailed, "by", caller.Username)
	return &Invited{Status: StatusInvited, Invitation: link}, nil
}

// campaignGroups returns the campaign's group for each role, or ErrNotFound
// if either is missing or is not really this campaign's.
func (m *Manager) campaignGroups(ctx context.Context, slug Slug) (map[Role]*authentik.Group, error) {
	groups := map[Role]*authentik.Group{}
	for _, role := range Roles {
		g, err := m.dir.Group(ctx, m.groupName(slug, role))
		if err != nil {
			return nil, err
		}
		if g == nil || checkGroup(g, slug, role) != nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, slug)
		}
		groups[role] = g
	}
	return groups, nil
}

// invitedRole is the role inv invites into campaign slug.
func (m *Manager) invitedRole(inv *authentik.Invitation, slug Slug) Role {
	for _, role := range Roles {
		if slices.Contains(inv.FixedData.Groups, m.groupName(slug, role)) {
			return role
		}
	}
	return ""
}

// invitationPrefix starts the name of every invitation this service makes,
// so Reconcile never touches anyone else's.
const invitationPrefix = "campaign-"

// invitationName is the invitation for email to campaign slug. It is the
// same every time, so asking again finds the first invitation instead of
// making another. The email is hashed, since names show in authentik's
// lists, and the result is a valid slug, which authentik requires.
func (m *Manager) invitationName(slug Slug, email string) string {
	sum := sha256.Sum256([]byte(email))
	return invitationPrefix + string(slug) + "-" + hex.EncodeToString(sum[:8])
}

// invitationURL is the link that redeems invitation pk.
func (m *Manager) invitationURL(pk string) string {
	u, err := url.Parse(m.opts.AuthentikURL)
	if err != nil {
		return ""
	}
	u = u.JoinPath("if", "flow", m.opts.InvitationFlow) // JoinPath drops the trailing slash
	u.Path += "/"
	u.RawQuery = url.Values{"itoken": {pk}}.Encode()
	return u.String()
}

// prefill is the sign-up form's suggestions from req.
func prefill(req InviteRequest) map[string]string {
	p := map[string]string{}
	if req.Username != "" {
		p["username"] = req.Username
	}
	if req.Name != "" {
		p["name"] = req.Name
	}
	return p
}

// parseEmail checks s is a bare email address, and lowercases it so the same
// person always gets the same invitation.
func parseEmail(s string) (string, error) {
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != strings.TrimSpace(s) {
		return "", fmt.Errorf("%w: %q is not an email address", ErrInvalid, s)
	}
	return strings.ToLower(addr.Address), nil
}

// Reconcile settles this service's invitations for people who have an
// account by now: they redeemed another campaign's invitation, or signed in
// through Google or Discord instead of using the link. Each such person joins
// the groups their invitation names, and the invitation is deleted, since the
// enrollment flow only ever makes new accounts. Expired invitations are
// deleted too. It carries on past a failed invitation, and reports every
// failure at the end.
func (m *Manager) Reconcile(ctx context.Context) error {
	invs, err := m.dir.Invitations(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, inv := range invs {
		if !strings.HasPrefix(inv.Name, invitationPrefix) {
			continue
		}
		if err := m.settle(ctx, inv); err != nil {
			errs = append(errs, fmt.Errorf("invitation %s: %w", inv.Name, err))
		}
	}
	return errors.Join(errs...)
}

// settle deals with one invitation for Reconcile.
func (m *Manager) settle(ctx context.Context, inv authentik.Invitation) error {
	if !inv.Expires.After(m.now()) {
		slog.InfoContext(ctx, "removing expired invitation", "invitation", inv.Name)
		return m.dir.DeleteInvitation(ctx, inv.Pk)
	}
	users, err := m.dir.UsersByEmail(ctx, inv.FixedData.Email)
	if err != nil {
		return err
	}
	if len(users) != 1 {
		// Nobody to settle yet, or an ambiguous email for a person to sort
		// out; either way the invitation stays.
		return nil
	}
	user := users[0]
	for _, name := range inv.FixedData.Groups {
		g, err := m.dir.Group(ctx, name)
		if err != nil {
			return err
		}
		if g == nil {
			return fmt.Errorf("group %s no longer exists", name)
		}
		if slices.Contains(g.Users, user.Pk) {
			continue
		}
		if err := m.dir.AddUserToGroup(ctx, g.Pk, user.Pk); err != nil {
			return err
		}
	}
	slog.InfoContext(ctx, "settled invitation for existing account", "invitation", inv.Name, "user", user.Pk)
	return m.dir.DeleteInvitation(ctx, inv.Pk)
}

// Reconciler runs Reconcile on a timer, as a service worker.
type Reconciler struct {
	manager  *Manager
	interval time.Duration
}

// NewReconciler returns a worker running m's Reconcile every interval.
func NewReconciler(m *Manager, interval time.Duration) *Reconciler {
	return &Reconciler{manager: m, interval: interval}
}

// Start reconciles until ctx is done. A failed run is logged and the next one
// tries again, so authentik being briefly away does not stop the service.
func (r *Reconciler) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if err := r.manager.Reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "reconciling invitations", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
