package command

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/pixil98/campaign-manager/internal/campaign"
)

func newWhoamiCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show who you are logged in as, and your campaigns",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, server, err := a.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			me, err := c.Me(cmd.Context())
			if err != nil {
				return explain(server, err)
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), me)
			}

			var b strings.Builder
			fmt.Fprintf(&b, "%s on %s\n", me.Username, server)
			if me.Admin {
				b.WriteString("An admin: you can create campaigns, and invite people into any of them.\n")
			}
			if len(me.Roles) == 0 {
				b.WriteString("Not in any campaigns.\n")
			} else {
				b.WriteString("\n")
				tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "CAMPAIGN\tROLE")
				for _, slug := range slices.Sorted(maps.Keys(me.Roles)) {
					_, _ = fmt.Fprintf(tw, "%s\t%s\n", slug, joinRoles(me.Roles[slug]))
				}
				_ = tw.Flush()
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), b.String())
			return err
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func joinRoles(roles []campaign.Role) string {
	s := make([]string, len(roles))
	for i, r := range roles {
		s[i] = string(r)
	}
	return strings.Join(s, ", ")
}

// withArticle reads "a mapper" or "an officer".
func withArticle(r campaign.Role) string {
	if strings.ContainsRune("aeiou", rune(r[0])) {
		return "an " + string(r)
	}
	return "a " + string(r)
}

func newCampaignCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "campaign",
		Short: "Manage campaigns",
	}
	cmd.AddCommand(newCampaignCreateCmd(a))
	return cmd
}

func newCampaignCreateCmd(a *app) *cobra.Command {
	var (
		name   string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "create <slug>",
		Short: "Create a campaign, or fill in what an existing one is missing",
		Long: `Create a campaign: its two groups in authentik, its rules in the wiki, and its
working pages, copied from the wiki's templates. Only admins can.

The slug names the campaign everywhere, in its group names and wiki
addresses, and cannot change once people are in it. It is lowercase letters
and digits, separated by single hyphens. The campaign's real name goes in
--name instead.

Creating a campaign that already exists adds only what it is missing, and
never overwrites its pages.`,
		Example: `  mpr campaign create dragons --name "Here Be Dragons"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, server, err := a.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			created, err := c.CreateCampaign(cmd.Context(), campaign.CreateRequest{Slug: args[0], Name: name})
			if err != nil {
				return explain(server, err)
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), created)
			}

			var b strings.Builder
			if created.New {
				fmt.Fprintf(&b, "Created campaign %s.\n", created.Slug)
			} else {
				fmt.Fprintf(&b, "Campaign %s already existed; added anything it was missing.\n", created.Slug)
			}
			for _, role := range campaign.Roles {
				if group, ok := created.Groups[role]; ok {
					fmt.Fprintf(&b, "  %s group: %s\n", role, group)
				}
			}
			if len(created.SeededPages) > 0 {
				fmt.Fprintf(&b, "  pages copied in: %s\n", strings.Join(created.SeededPages, ", "))
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), b.String())
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "the campaign's real name, kept on its groups")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newInviteCmd(a *app) *cobra.Command {
	var (
		req    campaign.InviteRequest
		role   string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "invite <campaign> <email>",
		Short: "Add someone to a campaign, or invite them if they have no account",
		Long: `Add someone to a campaign in a role. Someone who already has an account is
added at once. Someone without one gets an invitation into this campaign, and
mpr prints the link to send them.

Admins can invite into any campaign, and officers into their own. Inviting
only ever adds: someone already in the campaign, or already invited to it,
in the other role is refused rather than moved.`,
		Example: `  mpr invite dragons someone@example.com
  mpr invite dragons someone@example.com --role officer --username someone`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Email, req.Role = args[1], campaign.Role(role)
			if !req.Role.Valid() {
				return fmt.Errorf("role %q must be mapper or officer", role)
			}
			c, server, err := a.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			invited, err := c.Invite(cmd.Context(), args[0], req)
			if err != nil {
				return explain(server, err)
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), invited)
			}

			var b strings.Builder
			switch inv := invited.Invitation; {
			case invited.Status == campaign.StatusAdded:
				fmt.Fprintf(&b, "%s already had an account, and is now in %s as %s.\n", req.Email, args[0], withArticle(req.Role))
			case inv == nil:
				fmt.Fprintf(&b, "%s: %s\n", req.Email, invited.Status)
			default:
				fmt.Fprintf(&b, "Invited %s into %s as %s, until %s.\n", req.Email, args[0], withArticle(req.Role), inv.Expires.Local().Format(time.RFC1123))
				if inv.Emailed {
					b.WriteString("authentik has emailed them this link:\n")
				} else {
					b.WriteString("Send them this link:\n")
				}
				fmt.Fprintf(&b, "\n    %s\n", inv.URL)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), b.String())
			return err
		},
	}
	cmd.Flags().StringVar(&role, "role", string(campaign.RoleMapper), "mapper or officer")
	cmd.Flags().StringVar(&req.Username, "username", "", "a username to suggest on a new account's sign-up form")
	cmd.Flags().StringVar(&req.Name, "name", "", "a full name to suggest on a new account's sign-up form")
	_ = cmd.RegisterFlagCompletionFunc("role", cobra.FixedCompletions(
		[]string{string(campaign.RoleMapper), string(campaign.RoleOfficer)}, cobra.ShellCompDirectiveNoFileComp))
	addJSONFlag(cmd, &asJSON)
	return cmd
}
