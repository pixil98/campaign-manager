package campaign

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pixil98/campaign-manager/internal/authentik"
)

var admin = Caller{Username: "root", Admin: true}

func TestCreate(t *testing.T) {
	m, dir, wiki := newTestManager(t)

	got, err := m.Create(context.Background(), admin, CreateRequest{Slug: "epic", Name: "Epic Local 1"})
	if err != nil {
		t.Fatalf("Create = %v", err)
	}

	want := &Created{
		Slug:        "epic",
		Groups:      map[Role]string{RoleMapper: "mapper-epic-mapper", RoleOfficer: "mapper-epic-officer"},
		SeededPages: []string{"epic:lists:people", "epic:notes"},
		New:         true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Create = %+v, want %+v", got, want)
	}
	for _, role := range Roles {
		name := "mapper-epic-" + string(role)
		g := dir.groups[name]
		if g == nil {
			t.Fatalf("group %s was not made", name)
		}
		wantAttrs := map[string]any{"campaign": "epic", "role": string(role), "display_name": "Epic Local 1"}
		if !reflect.DeepEqual(g.Attributes, wantAttrs) {
			t.Errorf("%s attributes = %v, want %v", name, g.Attributes, wantAttrs)
		}
		if parent := dir.parents[name]; parent != dir.groups["mapper"].Pk {
			t.Errorf("%s parent = %q, want the mapper group", name, parent)
		}
	}
	wantACL := map[string]int{"epic:* @mapper-epic-mapper": 1, "epic:* @mapper-epic-officer": 16}
	if !reflect.DeepEqual(wiki.acl, wantACL) {
		t.Errorf("wiki rules = %v, want %v", wiki.acl, wantACL)
	}
	if wiki.pages["epic:notes"] != "notes template" {
		t.Errorf("epic:notes = %q, want the template's text", wiki.pages["epic:notes"])
	}
	if _, ok := wiki.pages["epic:intro"]; ok {
		t.Error("the handbook was copied into the campaign")
	}
}

// TestCreateAgain checks repeating Create, as after a failed run, only adds
// what is missing and never overwrites a page the campaign has edited.
func TestCreateAgain(t *testing.T) {
	m, dir, wiki := newTestManager(t)
	dir.addCampaign("epic")
	wiki.pages["epic:notes"] = "edited by the campaign"

	got, err := m.Create(context.Background(), admin, CreateRequest{Slug: "epic"})
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	if got.New {
		t.Error("Create reported a new campaign, but its groups already existed")
	}
	if want := []string{"epic:lists:people"}; !reflect.DeepEqual(got.SeededPages, want) {
		t.Errorf("seeded %v, want %v", got.SeededPages, want)
	}
	if wiki.pages["epic:notes"] != "edited by the campaign" {
		t.Errorf("epic:notes = %q, want the campaign's edit kept", wiki.pages["epic:notes"])
	}
	if len(wiki.acl) != 2 {
		t.Errorf("wiki rules = %v, want both added", wiki.acl)
	}
}

func TestCreateRefused(t *testing.T) {
	tests := map[string]struct {
		caller  Caller
		slug    string
		setup   func(*fakeDir)
		wantErr error
		wantMsg string
	}{
		"not an admin": {
			caller:  Caller{Roles: map[Slug][]Role{"epic": {RoleOfficer}}},
			slug:    "epic",
			wantErr: ErrForbidden,
		},
		"a bad slug": {
			caller:  admin,
			slug:    "Epic",
			wantErr: ErrInvalid,
		},
		"a group of that name belongs to something else": {
			caller:  admin,
			slug:    "epic",
			setup:   func(d *fakeDir) { d.addGroup("mapper-epic-mapper", map[string]any{}) },
			wantErr: ErrConflict,
			wantMsg: "mapper-epic-mapper already exists",
		},
		"no base group": {
			caller:  admin,
			slug:    "epic",
			setup:   func(d *fakeDir) { delete(d.groups, "mapper") },
			wantMsg: `base group "mapper" not found`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m, dir, wiki := newTestManager(t)
			if tt.setup != nil {
				tt.setup(dir)
			}
			_, err := m.Create(context.Background(), tt.caller, CreateRequest{Slug: tt.slug})
			if err == nil {
				t.Fatal("Create = nil, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Create = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Create = %q, want it to mention %q", err, tt.wantMsg)
			}
			if len(wiki.acl) != 0 {
				t.Errorf("wiki rules were added for a refused campaign: %v", wiki.acl)
			}
		})
	}
}

// TestInviteExistingAccount checks a person who already has an account
// joins the campaign at once, keeping every other campaign they are in.
func TestInviteExistingAccount(t *testing.T) {
	m, dir, _ := newTestManager(t)
	dir.addCampaign("epic")
	dir.addCampaign("other")
	alice := dir.addUser("Alice@Example.com")
	if err := dir.AddUserToGroup(context.Background(), dir.groups["mapper-other-officer"].Pk, alice.Pk); err != nil {
		t.Fatal(err)
	}

	got, err := m.Invite(context.Background(), admin, "epic", InviteRequest{Email: "alice@example.com", Role: RoleMapper})
	if err != nil {
		t.Fatalf("Invite = %v", err)
	}
	if want := (&Invited{Status: StatusAdded}); !reflect.DeepEqual(got, want) {
		t.Errorf("Invite = %+v, want %+v", got, want)
	}
	want := []string{"mapper-epic-mapper", "mapper-other-officer"}
	if got := dir.members(alice.Pk); !reflect.DeepEqual(got, want) {
		t.Errorf("Alice is in %v, want %v", got, want)
	}
	if len(dir.invitations) != 0 {
		t.Errorf("an invitation was made for someone with an account: %v", dir.invitations)
	}
}

// TestInviteNewPerson checks a person with no account gets an invitation for
// the campaign, which the enrollment flow turns into an account in its group.
func TestInviteNewPerson(t *testing.T) {
	m, dir, _ := newTestManager(t)
	dir.addCampaign("epic")

	req := InviteRequest{Email: "Bob@Example.com", Role: RoleOfficer, Username: "bob", Name: "Bob B"}
	got, err := m.Invite(context.Background(), admin, "epic", req)
	if err != nil {
		t.Fatalf("Invite = %v", err)
	}
	if len(dir.invitations) != 1 {
		t.Fatalf("invitations = %v, want one", dir.invitations)
	}
	var inv *authentik.Invitation
	for _, i := range dir.invitations {
		inv = i
	}
	if !strings.HasPrefix(inv.Name, "campaign-epic-") {
		t.Errorf("invitation name = %q, want it to start campaign-epic-", inv.Name)
	}
	if strings.Contains(inv.Name, "bob") {
		t.Errorf("invitation name %q shows the email", inv.Name)
	}
	wantData := authentik.InvitationData{
		Email:   "bob@example.com",
		Groups:  []string{"mapper-epic-officer"},
		Prefill: map[string]string{"username": "bob", "name": "Bob B"},
	}
	if !reflect.DeepEqual(inv.FixedData, wantData) {
		t.Errorf("fixed_data = %+v, want %+v", inv.FixedData, wantData)
	}
	if !inv.SingleUse || inv.Flow != "flow-pk" || !inv.Expires.Equal(testNow.Add(testLifetime)) {
		t.Errorf("invitation = %+v, want single use, the campaign flow, and expiring after the lifetime", inv)
	}
	want := &Invited{Status: StatusInvited, Invitation: &InvitationLink{
		URL:     "https://auth.example.com/if/flow/campaign-enrollment/?itoken=" + inv.Pk,
		Expires: testNow.Add(testLifetime),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Invite = %+v, want %+v", got.Invitation, want.Invitation)
	}
	if len(dir.sent) != 0 {
		t.Errorf("emailed %v with emailing off", dir.sent)
	}

	// Asking again finds the same invitation rather than making another.
	again, err := m.Invite(context.Background(), admin, "epic", req)
	if err != nil {
		t.Fatalf("Invite again = %v", err)
	}
	if len(dir.invitations) != 1 || again.Invitation.URL != got.Invitation.URL {
		t.Errorf("asking again made another invitation: %v", dir.invitations)
	}
}

// TestInviteSeparatePerCampaign checks inviting one new person into two
// campaigns makes two invitations, each naming only its own campaign. A
// shared one would hand each inviter a link into the other campaign.
func TestInviteSeparatePerCampaign(t *testing.T) {
	m, dir, _ := newTestManager(t)
	dir.addCampaign("epic")
	dir.addCampaign("other")

	for _, slug := range []string{"epic", "other"} {
		if _, err := m.Invite(context.Background(), admin, slug, InviteRequest{Email: "carol@example.com", Role: RoleMapper}); err != nil {
			t.Fatalf("Invite into %s = %v", slug, err)
		}
	}
	if len(dir.invitations) != 2 {
		t.Fatalf("invitations = %v, want one per campaign", dir.invitations)
	}
	for name, inv := range dir.invitations {
		if len(inv.FixedData.Groups) != 1 {
			t.Errorf("%s names groups %v, want only its own campaign's", name, inv.FixedData.Groups)
		}
		slug := strings.Split(strings.TrimPrefix(name, invitationPrefix), "-")[0]
		if want := "mapper-" + slug + "-mapper"; inv.FixedData.Groups[0] != want {
			t.Errorf("%s names %v, want %s", name, inv.FixedData.Groups, want)
		}
	}
}

func TestInviteEmails(t *testing.T) {
	m, dir, _ := newTestManager(t)
	m.opts.SendInvitationEmail = true
	dir.addCampaign("epic")

	got, err := m.Invite(context.Background(), admin, "epic", InviteRequest{Email: "dave@example.com", Role: RoleMapper})
	if err != nil {
		t.Fatalf("Invite = %v", err)
	}
	if !got.Invitation.Emailed || len(dir.sent) != 1 || !strings.HasSuffix(dir.sent[0], " to dave@example.com") {
		t.Errorf("emailed = %v, sent %v; want the invitation emailed to dave", got.Invitation.Emailed, dir.sent)
	}
}

// TestInviteReplacesExpired checks an expired invitation is replaced by a
// fresh one, rather than handing out a link that no longer works.
func TestInviteReplacesExpired(t *testing.T) {
	m, dir, _ := newTestManager(t)
	dir.addCampaign("epic")
	req := InviteRequest{Email: "erin@example.com", Role: RoleMapper}
	first, err := m.Invite(context.Background(), admin, "epic", req)
	if err != nil {
		t.Fatal(err)
	}

	m.now = func() time.Time { return testNow.Add(testLifetime + time.Hour) }
	second, err := m.Invite(context.Background(), admin, "epic", req)
	if err != nil {
		t.Fatalf("Invite after expiry = %v", err)
	}
	if second.Invitation.URL == first.Invitation.URL || len(dir.invitations) != 1 {
		t.Errorf("after expiry, got %s and invitations %v; want one new invitation", second.Invitation.URL, dir.invitations)
	}
}

func TestInviteRefused(t *testing.T) {
	officerOfEpic := Caller{Username: "olive", Roles: map[Slug][]Role{"epic": {RoleOfficer}, "other": {RoleMapper}}}
	tests := map[string]struct {
		caller  Caller
		slug    string
		req     InviteRequest
		setup   func(*Manager, *fakeDir)
		wantErr error
		wantMsg string
	}{
		"an officer, into a campaign they are only a member of": {
			caller:  officerOfEpic,
			slug:    "other",
			req:     InviteRequest{Email: "x@example.com", Role: RoleMapper},
			wantErr: ErrForbidden,
		},
		"an officer, into a campaign they are not in": {
			caller:  officerOfEpic,
			slug:    "third",
			req:     InviteRequest{Email: "x@example.com", Role: RoleMapper},
			wantErr: ErrForbidden,
		},
		"a member": {
			caller:  Caller{Roles: map[Slug][]Role{"epic": {RoleMapper}}},
			slug:    "epic",
			req:     InviteRequest{Email: "x@example.com", Role: RoleMapper},
			wantErr: ErrForbidden,
		},
		"no such campaign": {
			caller:  admin,
			slug:    "nope",
			req:     InviteRequest{Email: "x@example.com", Role: RoleMapper},
			wantErr: ErrNotFound,
		},
		"no role": {
			caller:  officerOfEpic,
			slug:    "epic",
			req:     InviteRequest{Email: "x@example.com"},
			wantErr: ErrInvalid,
		},
		"not an email address": {
			caller:  officerOfEpic,
			slug:    "epic",
			req:     InviteRequest{Email: "Xavier <x@example.com>", Role: RoleMapper},
			wantErr: ErrInvalid,
		},
		"already in the campaign in another role": {
			caller: officerOfEpic,
			slug:   "epic",
			req:    InviteRequest{Email: "frank@example.com", Role: RoleOfficer},
			setup: func(_ *Manager, d *fakeDir) {
				u := d.addUser("frank@example.com")
				_ = d.AddUserToGroup(context.Background(), d.groups["mapper-epic-mapper"].Pk, u.Pk)
			},
			wantErr: ErrConflict,
			wantMsg: "already in the campaign as mapper",
		},
		"already invited to the campaign in another role": {
			caller: officerOfEpic,
			slug:   "epic",
			req:    InviteRequest{Email: "gina@example.com", Role: RoleOfficer},
			setup: func(m *Manager, _ *fakeDir) {
				_, _ = m.Invite(context.Background(), admin, "epic", InviteRequest{Email: "gina@example.com", Role: RoleMapper})
			},
			wantErr: ErrConflict,
			wantMsg: "already invited to the campaign as mapper",
		},
		"two accounts with the email": {
			caller: officerOfEpic,
			slug:   "epic",
			req:    InviteRequest{Email: "hal@example.com", Role: RoleMapper},
			setup: func(_ *Manager, d *fakeDir) {
				d.addUser("hal@example.com")
				d.addUser("HAL@example.com")
			},
			wantErr: ErrConflict,
			wantMsg: "2 accounts",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m, dir, _ := newTestManager(t)
			dir.addCampaign("epic")
			dir.addCampaign("other")
			if tt.setup != nil {
				tt.setup(m, dir)
			}
			_, err := m.Invite(context.Background(), tt.caller, tt.slug, tt.req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Invite = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Invite = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// TestInviteByOfficer checks an officer can invite into their own campaign
// in either role.
func TestInviteByOfficer(t *testing.T) {
	m, dir, _ := newTestManager(t)
	dir.addCampaign("epic")
	officer := Caller{Username: "olive", Roles: map[Slug][]Role{"epic": {RoleOfficer}}}
	for _, role := range Roles {
		if _, err := m.Invite(context.Background(), officer, "epic", InviteRequest{Email: string(role) + "@example.com", Role: role}); err != nil {
			t.Errorf("officer inviting a %s = %v", role, err)
		}
	}
}

// TestReconcile checks a person with an invitation from each of two
// campaigns, who redeemed neither but has an account by now, ends up in both
// campaigns, and both invitations are gone.
func TestReconcile(t *testing.T) {
	m, dir, _ := newTestManager(t)
	dir.addCampaign("epic")
	dir.addCampaign("other")
	ctx := context.Background()
	if _, err := m.Invite(ctx, admin, "epic", InviteRequest{Email: "ivy@example.com", Role: RoleOfficer}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Invite(ctx, admin, "other", InviteRequest{Email: "ivy@example.com", Role: RoleMapper}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Invite(ctx, admin, "epic", InviteRequest{Email: "jack@example.com", Role: RoleMapper}); err != nil {
		t.Fatal(err)
	}
	// Someone else's invitation, which Reconcile must leave alone even for
	// a person with an account.
	dir.invitations["general"] = &authentik.Invitation{Pk: "general-pk", Name: "general", Expires: testNow.Add(time.Hour),
		FixedData: authentik.InvitationData{Email: "ivy@example.com", Groups: []string{"mapper-epic-mapper"}}}

	ivy := dir.addUser("Ivy@example.com")
	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}

	if got, want := dir.members(ivy.Pk), []string{"mapper-epic-officer", "mapper-other-mapper"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Ivy is in %v, want %v", got, want)
	}
	var left []string
	for name := range dir.invitations {
		left = append(left, name)
	}
	if len(left) != 2 || dir.invitations["general"] == nil {
		t.Errorf("invitations left = %v, want only Jack's and the general one", left)
	}
}

func TestReconcileRemovesExpired(t *testing.T) {
	m, dir, _ := newTestManager(t)
	dir.addCampaign("epic")
	if _, err := m.Invite(context.Background(), admin, "epic", InviteRequest{Email: "kim@example.com", Role: RoleMapper}); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return testNow.Add(testLifetime + time.Minute) }
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if len(dir.invitations) != 0 {
		t.Errorf("invitations = %v, want the expired one removed", dir.invitations)
	}
}
