// Package campaign creates campaigns and invites people into them.
//
// A campaign has no record of its own. It is a slug that authentik, Mapper
// and the wiki agree on: two authentik groups named after it, which Mapper
// reads roles from, and two wiki ACL rules on the namespace named after it.
// Who is in a campaign is only ever recorded as membership of its groups.
package campaign

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Errors a request can fail with, which the API turns into status codes.
// Each is wrapped with the details.
var (
	ErrInvalid   = errors.New("invalid request")
	ErrForbidden = errors.New("not allowed")
	ErrNotFound  = errors.New("no such campaign")
	ErrConflict  = errors.New("conflict")
)

// Role is what a person does in one campaign. Its values are the suffixes of
// the campaign's group names, which Mapper's group patterns match, so they
// cannot change.
type Role string

// Role values.
const (
	RoleMapper  Role = "mapper"
	RoleOfficer Role = "officer"
)

// Roles is every role, in the order a campaign's groups are made.
var Roles = []Role{RoleMapper, RoleOfficer}

// Valid reports whether r is one of the roles.
func (r Role) Valid() bool {
	return slices.Contains(Roles, r)
}

// wikiLevel is the DokuWiki ACL level each role gets on its campaign's
// namespace: read for a member, and delete for an officer, which includes
// upload, create and edit.
func (r Role) wikiLevel() int {
	if r == RoleOfficer {
		return 16
	}
	return 1
}

// Slug names a campaign everywhere it appears: its group names, its wiki
// namespace and URLs. It never changes once a campaign has people in it,
// since renaming a group reassigns its members.
type Slug string

// slugPattern keeps a slug to lowercase letters, digits and single hyphens,
// which DokuWiki's page names and Mapper's group patterns both take as is.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// maxSlugLength keeps group names and wiki ids short enough to read.
const maxSlugLength = 40

// reservedSlugs are wiki namespaces that already mean something, and "admin",
// whose groups would read like the cluster-wide mapper-admin group.
var reservedSlugs = []string{"admin", "handbook", "playground", "templates", "wiki"}

// ParseSlug checks s can name a new campaign.
func ParseSlug(s string) (Slug, error) {
	if !slugPattern.MatchString(s) {
		return "", fmt.Errorf("%w: slug %q must be lowercase letters and digits, separated by single hyphens", ErrInvalid, s)
	}
	if len(s) > maxSlugLength {
		return "", fmt.Errorf("%w: slug %q is longer than %d characters", ErrInvalid, s, maxSlugLength)
	}
	if slices.Contains(reservedSlugs, s) {
		return "", fmt.Errorf("%w: slug %q is reserved", ErrInvalid, s)
	}
	// A role as one of its parts would let Mapper's group patterns read
	// mapper-x-officer-mapper as either campaign.
	for _, part := range strings.Split(s, "-") {
		if Role(part).Valid() {
			return "", fmt.Errorf("%w: slug %q cannot contain %q as one of its parts", ErrInvalid, s, part)
		}
	}
	return Slug(s), nil
}

// Caller is who is asking, as their authentik token describes them.
type Caller struct {
	Subject  string `json:"subject"`
	Username string `json:"username"`
	// Admin is a cluster-wide admin, who can do anything in any campaign.
	Admin bool `json:"admin"`
	// Roles is every campaign the caller is in and their roles in it. A
	// person can be in several campaigns, with a different role in each, so
	// what they may do is always decided for one campaign at a time.
	Roles map[Slug][]Role `json:"roles"`
}

// Has reports whether the caller holds role in campaign slug.
func (c Caller) Has(slug Slug, role Role) bool {
	return slices.Contains(c.Roles[slug], role)
}

// canInvite reports whether the caller may add people to campaign slug: an
// admin anywhere, or an officer of that campaign. Being an officer of one
// campaign gives nothing in another.
func (c Caller) canInvite(slug Slug) bool {
	return c.Admin || c.Has(slug, RoleOfficer)
}
