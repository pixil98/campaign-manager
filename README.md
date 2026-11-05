# campaign-manager

Creates campaigns and invites people into them, across authentik and the
wiki. `mpr` is its command line.

A campaign has no record of its own. It is a slug that three systems agree on:

- **authentik**: two groups under the base group, `mapper-<slug>-mapper` and
  `mapper-<slug>-officer`, with `campaign` and `role` attributes. Membership
  of these is the only record of who is in a campaign.
- **Mapper**: nothing to create. It reads campaign and role from the group
  names at login.
- **The wiki**: two ACL rules on the `<slug>:` namespace (read for members,
  everything up to delete for officers), plus the working pages copied from
  `templates:`.

## Who can do what

Callers send an authentik access token for this service's provider as a
bearer token, which `mpr login` gets them. The service checks the token's
signature, issuer and audience, then asks authentik's userinfo endpoint about
it, so a token stops working as soon as it is revoked or its user is
deactivated. Roles come from the userinfo's `campaigns` and `mapper_admin`
claims, which authentik builds from the groups' attributes on every request,
so a change to someone's groups counts at once, however long their token
lasts.

A person can be in several campaigns with a different role in each, so every
check is made for one campaign:

| Caller | Create a campaign | Invite into campaign X |
|--------|-------------------|------------------------|
| `mapper-admin` | yes | yes, in either role |
| officer of X | no | yes, in either role |
| member of X, or officer only of other campaigns | no | no |

Invites only ever add. Someone already in the campaign, or already invited
to it, in another role is refused with 409 rather than having their role
changed.

## API

| Request | Body | Answer |
|---------|------|--------|
| `GET /healthz` | | `ok`, no token needed |
| `GET /auth` | | how to get a token: `issuer`, `client_id` and `scopes`. No token needed |
| `GET /me` | | the caller, as authentik describes them now |
| `POST /campaigns` | `{"slug": "dragons", "name": "Here Be Dragons"}` | 201 with the groups and the pages copied in; 200 if the campaign already existed and only missing pieces were added |
| `POST /campaigns/{slug}/members` | `{"email": "...", "role": "mapper" or "officer", "username": "...", "name": "..."}` | 200 with `status` `added` or `invited` |

`username` and `name` are optional suggestions a new account's sign-up form
starts with.

Creating a campaign finds before it makes at every step, so a failed or
repeated call is safe. Pages the campaign already has are never overwritten.

Inviting someone who already has an account adds them to the campaign's
group at once, leaving their other campaigns alone. Inviting someone without
one creates an authentik invitation for that person and that campaign only,
and answers with its link:

```json
{"status": "invited", "invitation": {"url": "https://auth.example.com/if/flow/campaign-enrollment/?itoken=...", "expires": "...", "emailed": false}}
```

Each campaign invites separately. A shared invitation would hand every
inviter a link into the other campaigns too. Once a person has an account,
because they redeemed one campaign's invitation or signed in with Google or
Discord instead, a background reconciler adds them to the groups of their
other pending invitations and deletes those. It also removes expired
invitations. It only touches invitations whose names start `campaign-`.

Errors are `{"error": "..."}` with 400 (bad request), 401 (no or bad token),
403 (not allowed), 404 (no such campaign), 409 (already in or invited) or 503
(authentik could not be asked about the token). Anything else is a 500 whose
details go to the log only.

## mpr

```sh
mpr login --server https://campaigns.example.com
mpr whoami
mpr campaign create dragons --name "Here Be Dragons"
mpr invite dragons someone@example.com --role officer
```

`mpr login` uses authentik's device code login. It prints a link, which can be
opened on any machine, and waits while you log in there. It asks the server
for the provider and scopes through `GET /auth`, so the server's address is
the only thing mpr needs to know, and the first server you log in to becomes
the default. `mpr logout` revokes the login's tokens at authentik, so they
stop working at once.

The server comes from `--server`, then `MPR_SERVER`, then the config file,
`mpr/config.json` in your config directory (`~/.config` on Linux), or
`--config` or `MPR_CONFIG` for another. `mpr config set server <url>` changes
it. Logins are kept in `credentials.json` beside the config, readable only by
you, one per server, and are refreshed and saved again as their access tokens
run out.

Results that scripts might read take `--json`. `mpr completion` prints shell
completions.

Releases for Linux, macOS and Windows are built by GoReleaser when a `v*` tag
is pushed. `go install github.com/pixil98/campaign-manager/cmd/mpr@latest`
works as well.

## Configuration

A JSON file given with `--config`; see `config.example.json`.

| Setting | |
|---------|---|
| `listen` | address to serve on, such as `:8080` |
| `oidc.issuer`, `oidc.client_id` | the authentik provider callers' tokens come from |
| `authentik.url` | authentik's **public** address. Invitation emails are built from the host authentik is called on, so the in-cluster Service would put internal links in them |
| `authentik.token` or `authentik.token_file` | API token for the service's authentik account, or a file holding it, such as a mounted Secret |
| `authentik.base_group` | the group campaign groups sit under, and the start of their names (`mapper`) |
| `authentik.invitation_flow` | slug of the enrollment flow invitations use |
| `authentik.invitation_lifetime` | how long an invitation lasts, such as `168h` |
| `authentik.send_invitation_email` | have authentik email invitations; when off, the link is only returned |
| `wiki.url` | the wiki; its in-cluster Service will do |
| `wiki.token` or `wiki.token_file` | DokuWiki API token for the service's wiki account, or a file holding it |
| `wiki.templates_namespace` | where working pages are copied from (`templates`) |
| `reconcile_interval` | how often pending invitations are settled, such as `1m` |

`--loglevel` and `--logformat` are go-service's.

## What it needs set up first

**In authentik:**

- An OAuth2 provider and application for the service. Callers are read
  from its userinfo endpoint, not from the token, so
  `include_claims_in_id_token` can be off. For `mpr`, the provider is a public
  client with the device code and refresh token grants, and the
  `offline_access` scope. Its `access_code_validity` is how long someone has
  to finish logging in, so a minute, the default, is too short.
- A device code flow on the brand. authentik's device page answers 404
  without one. An empty flow will do; authentik adds the code entry itself.
- A scope mapping on that provider emitting `campaigns` and `mapper_admin`,
  built from the campaign groups' `campaign` and `role` attributes, as
  Mapper's own groups mapping is.
- A service account with an API token, and a role that can read users,
  create groups and change their membership, and create, send and delete
  invitations.
- The enrollment flow invitations use, with an invitation stage that
  continues without an invitation off, so only invitees can sign up.

**In the wiki:**

- `remote = 1` and `remoteuser` naming the service's account, in DokuWiki's
  `conf/local.protected.php`. The account must also be in
  `superuser`, since only a superuser can add ACL rules.
- An entry for the account in `conf/users.auth.php`, with its group. The
  password field can be `!`: DokuWiki checks the API token before any other
  login, so the account never needs a password.
- A token, minted once from inside the pod:

  ```sh
  php -r 'define("DOKU_INC", "/var/www/html/"); require DOKU_INC . "inc/init.php";
          echo \dokuwiki\JWT::fromUser("campaign-manager")->getToken();'
  ```

  DokuWiki keeps one token per account and returns the stored one while it
  is valid. The token is standard base64 and can contain `+`, `/` and `=`,
  so copy it whole.

## Development

```sh
go test -race ./...
```

`internal/dokuwiki` has a live test against a real wiki. It needs a
superuser's API token, and works in a namespace of its own that it cleans up
afterwards:

```sh
DOKUWIKI_URL=http://localhost:8080 DOKUWIKI_TOKEN=... go test -run TestLive ./internal/dokuwiki
```

Identifiers use `Id`, not `ID`, as in go-mud.
