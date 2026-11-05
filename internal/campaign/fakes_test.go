package campaign

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pixil98/campaign-manager/internal/authentik"
)

// testNow is the time every test manager runs at.
var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// testLifetime is how long test invitations last.
const testLifetime = 7 * 24 * time.Hour

// newTestManager returns a manager over an authentik with only the base
// group, and a wiki with templates, both empty of campaigns.
func newTestManager(t *testing.T) (*Manager, *fakeDir, *fakeWiki) {
	t.Helper()
	dir := &fakeDir{
		groups:      map[string]*authentik.Group{},
		invitations: map[string]*authentik.Invitation{},
		flows:       map[string]string{"campaign-enrollment": "flow-pk"},
	}
	dir.addGroup("mapper", nil)
	wiki := &fakeWiki{
		acl: map[string]int{},
		pages: map[string]string{
			"templates:notes":        "notes template",
			"templates:lists:people": "people template",
			"handbook:intro":         "the handbook",
		},
	}
	m := NewManager(dir, wiki, Options{
		BaseGroup:          "mapper",
		AuthentikURL:       "https://auth.example.com",
		InvitationFlow:     "campaign-enrollment",
		InvitationLifetime: testLifetime,
		TemplatesNamespace: "templates",
	})
	m.now = func() time.Time { return testNow }
	return m, dir, wiki
}

// addCampaign makes the groups Create would have made for slug.
func (d *fakeDir) addCampaign(slug string) {
	for _, role := range Roles {
		d.addGroup("mapper-"+slug+"-"+string(role), map[string]any{"campaign": slug, "role": string(role)})
	}
}

// fakeDir is an authentik held in memory.
type fakeDir struct {
	groups      map[string]*authentik.Group
	users       []authentik.User
	invitations map[string]*authentik.Invitation
	flows       map[string]string
	sent        []string
	nextPk      int
	// parents records each created group's parent.
	parents map[string]string
}

func (d *fakeDir) newPk(kind string) string {
	d.nextPk++
	return fmt.Sprintf("%s-%d", kind, d.nextPk)
}

func (d *fakeDir) addGroup(name string, attrs map[string]any) *authentik.Group {
	g := &authentik.Group{Pk: d.newPk("group"), Name: name, Attributes: attrs}
	d.groups[name] = g
	return g
}

func (d *fakeDir) addUser(email string) authentik.User {
	u := authentik.User{Pk: len(d.users) + 1, Username: strings.Split(email, "@")[0], Email: email}
	d.users = append(d.users, u)
	return u
}

// members is the names of the groups user is directly in.
func (d *fakeDir) members(user int) []string {
	var names []string
	for name, g := range d.groups {
		if slices.Contains(g.Users, user) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (d *fakeDir) Group(_ context.Context, name string) (*authentik.Group, error) {
	g, ok := d.groups[name]
	if !ok {
		return nil, nil
	}
	cp := *g
	cp.Users = slices.Clone(g.Users)
	return &cp, nil
}

func (d *fakeDir) CreateGroup(_ context.Context, name, parent string, attributes map[string]any) (*authentik.Group, error) {
	if _, ok := d.groups[name]; ok {
		return nil, fmt.Errorf("group %s exists", name)
	}
	g := d.addGroup(name, attributes)
	if d.parents == nil {
		d.parents = map[string]string{}
	}
	d.parents[name] = parent
	return g, nil
}

func (d *fakeDir) UsersByEmail(_ context.Context, email string) ([]authentik.User, error) {
	var found []authentik.User
	for _, u := range d.users {
		if strings.EqualFold(u.Email, email) {
			found = append(found, u)
		}
	}
	return found, nil
}

func (d *fakeDir) AddUserToGroup(_ context.Context, group string, user int) error {
	for _, g := range d.groups {
		if g.Pk == group {
			if !slices.Contains(g.Users, user) {
				g.Users = append(g.Users, user)
			}
			return nil
		}
	}
	return fmt.Errorf("no group %s", group)
}

func (d *fakeDir) Invitation(_ context.Context, name string) (*authentik.Invitation, error) {
	inv, ok := d.invitations[name]
	if !ok {
		return nil, nil
	}
	cp := *inv
	return &cp, nil
}

func (d *fakeDir) Invitations(_ context.Context) ([]authentik.Invitation, error) {
	var all []authentik.Invitation
	for _, inv := range d.invitations {
		all = append(all, *inv)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

func (d *fakeDir) CreateInvitation(_ context.Context, inv authentik.Invitation) (*authentik.Invitation, error) {
	if _, ok := d.invitations[inv.Name]; ok {
		return nil, fmt.Errorf("invitation %s exists", inv.Name)
	}
	inv.Pk = d.newPk("invitation")
	d.invitations[inv.Name] = &inv
	cp := inv
	return &cp, nil
}

func (d *fakeDir) DeleteInvitation(_ context.Context, pk string) error {
	for name, inv := range d.invitations {
		if inv.Pk == pk {
			delete(d.invitations, name)
			return nil
		}
	}
	return fmt.Errorf("no invitation %s", pk)
}

func (d *fakeDir) SendInvitation(_ context.Context, pk, email string) error {
	d.sent = append(d.sent, pk+" to "+email)
	return nil
}

func (d *fakeDir) FlowPk(_ context.Context, slug string) (string, error) {
	pk, ok := d.flows[slug]
	if !ok {
		return "", fmt.Errorf("no flow %s", slug)
	}
	return pk, nil
}

// fakeWiki is a wiki held in memory. acl is keyed "scope subject".
type fakeWiki struct {
	acl   map[string]int
	pages map[string]string
}

func (w *fakeWiki) AddACL(_ context.Context, scope, subject string, level int) error {
	w.acl[scope+" "+subject] = level
	return nil
}

func (w *fakeWiki) ListPages(_ context.Context, namespace string) ([]string, error) {
	var ids []string
	for id := range w.pages {
		if strings.HasPrefix(id, namespace+":") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (w *fakeWiki) Page(_ context.Context, id string) (string, error) {
	return w.pages[id], nil
}

func (w *fakeWiki) SavePage(_ context.Context, id, text, _ string) error {
	w.pages[id] = text
	return nil
}
