package campaign

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSlug(t *testing.T) {
	tests := map[string]struct {
		slug    string
		wantErr string
	}{
		"a word":                     {slug: "epic"},
		"words and digits":           {slug: "local-1"},
		"as long as allowed":         {slug: strings.Repeat("a", maxSlugLength)},
		"uppercase":                  {slug: "Epic", wantErr: "lowercase letters and digits"},
		"a space":                    {slug: "two words", wantErr: "lowercase letters and digits"},
		"a double hyphen":            {slug: "a--b", wantErr: "single hyphens"},
		"a leading hyphen":           {slug: "-a", wantErr: "single hyphens"},
		"a trailing hyphen":          {slug: "a-", wantErr: "single hyphens"},
		"empty":                      {slug: "", wantErr: "lowercase letters and digits"},
		"too long":                   {slug: strings.Repeat("a", maxSlugLength+1), wantErr: "longer than"},
		"a wiki namespace in use":    {slug: "handbook", wantErr: "reserved"},
		"like the admin group":       {slug: "admin", wantErr: "reserved"},
		"a role as its last part":    {slug: "x-officer", wantErr: `cannot contain "officer"`},
		"a role as its first part":   {slug: "mapper-x", wantErr: `cannot contain "mapper"`},
		"a role inside a word is ok": {slug: "officers"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseSlug(tt.slug)
			if tt.wantErr == "" {
				if err != nil || got != Slug(tt.slug) {
					t.Fatalf("ParseSlug(%q) = %q, %v; want %q", tt.slug, got, err, tt.slug)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseSlug(%q) = %q, want an error mentioning %q", tt.slug, got, tt.wantErr)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("ParseSlug(%q) = %v, want it to be ErrInvalid", tt.slug, err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseSlug(%q) = %q, want it to mention %q", tt.slug, err, tt.wantErr)
			}
		})
	}
}

// TestCallerCanInvite checks inviting is decided one campaign at a time: an
// officer of one campaign who is only a member of another can invite into
// the first and not the second.
func TestCallerCanInvite(t *testing.T) {
	both := Caller{Roles: map[Slug][]Role{"epic": {RoleOfficer}, "other": {RoleMapper}}}
	tests := map[string]struct {
		caller Caller
		slug   Slug
		want   bool
	}{
		"an admin, anywhere":                    {caller: Caller{Admin: true}, slug: "epic", want: true},
		"an officer, in their campaign":         {caller: both, slug: "epic", want: true},
		"an officer, where they are a member":   {caller: both, slug: "other", want: false},
		"an officer, where they are not at all": {caller: both, slug: "third", want: false},
		"a member":                              {caller: Caller{Roles: map[Slug][]Role{"epic": {RoleMapper}}}, slug: "epic", want: false},
		"nobody":                                {caller: Caller{}, slug: "epic", want: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.caller.canInvite(tt.slug); got != tt.want {
				t.Errorf("canInvite(%q) = %v, want %v", tt.slug, got, tt.want)
			}
		})
	}
}
