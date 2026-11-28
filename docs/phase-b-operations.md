# Phase B operations guide

PHOENIX Phase B adds DNS-01 certificates, cached routing rule-set catalogs,
protected Prometheus metrics, per-administrator dashboard preferences, and
standards-based OpenID Connect (OIDC) sign-in. All database changes are
additive: an existing installation keeps its panel settings, users, sessions,
API tokens, clients, resellers, and default dashboard until an administrator
opts into a new feature.

## DNS-01 and wildcard certificates

Open **Settings → Security → DNS-01 certificates** and create a profile.

- **Manual DNS TXT** is portable across DNS providers. Start *Issue / renew*,
  publish every displayed `_acme-challenge` TXT record exactly as shown, wait
  for propagation, then use *Verify TXT*. PHOENIX stores no DNS credential for
  this mode.
- **Cloudflare API token** is the first automated adapter. Use a scoped token
  with Zone Read and DNS Edit only for the relevant zones. The token is
  encrypted at rest and is never returned by the API or UI. Enabled Cloudflare
  profiles are checked by the normal daily certificate renewal job.
- Wildcards, such as `*.example.com`, require DNS-01 and can be included with
  their apex name (`example.com`) in a single profile. One profile can contain
  up to 20 DNS names. At most one profile can have **Apply to panel** enabled,
  because the panel has one active certificate/key pair.

Only an interactively signed-in, recently re-authenticated superadministrator
can create, change, issue, complete, or delete profiles. A manual challenge
expires after two hours. Keep a working local administrator login while testing
new TLS or SSO settings.

## Rule-set catalog

Open **Xray → Routing → Rule-set catalog**.

Catalog sources must be absolute HTTPS URLs on port 443. PHOENIX refuses URLs
with credentials or fragments, local/private/non-public targets, unsafe
redirects, proxy use, payloads above 4 MiB, and lists with more than 100,000
entries. DNS is checked both before fetching and at connection time. Failed
refreshes preserve the last known-good cache.

A catalog is data only: it cannot execute configuration or silently replace an
Xray template. Use **Inspect** to review its normalized cached entries and
digest. **Create editable routing rule** copies the reviewed current entries
into the ordinary routing-rule editor; select the outbound/balancer there and
save the Xray template normally. Later catalog refreshes never modify an
already-created rule. For reviewability and manageable Xray configurations,
the UI limits one copied rule to 10,000 entries.

Creating, refreshing, editing, and deleting a catalog require recent
interactive superadministrator authentication and are recorded by the panel
audit log.

## Prometheus

The canonical scrape endpoint is:

```text
https://panel.example.invalid/<base-path>/metrics
```

It is intentionally not public. Authenticate either with a current panel
browser session or a bearer API token. Legacy `full` and `read` tokens retain
their existing read access; a least-privilege token needs only `metrics:read`.
For API explorers, `/panel/api/metrics` is an equivalent alias.

Example:

```bash
curl --fail --header "Authorization: Bearer ${PHOENIX_METRICS_TOKEN}" \
  https://panel.example.invalid/metrics
```

Metrics contain host/resource totals and service state only; they do not expose
subscriber addresses, credentials, client identifiers, or panel secrets.

## Dashboard preferences

Use **Customize** on the dashboard to select visible administrator widgets,
change their order, and choose a live refresh interval from 2 to 60 seconds.
Preferences are stored per administrator. Missing rows retain the prior
built-in dashboard and a safe two-second refresh default, so upgrades do not
change existing users' dashboard behavior.

## OIDC single sign-on

Open **Settings → Security → Single sign-on (OIDC)**. Configure the HTTPS
issuer URL and client ID registered at your identity provider. Configure a
client secret for confidential clients; public clients may use PKCE without a
secret when their provider allows it. The callback URL to register is:

```text
https://panel.example.invalid/<base-path>/sso/oidc/callback
```

PHOENIX uses OIDC Discovery with Authorization Code + PKCE. It validates state,
nonce, issuer, audience, expiry, issued-at time, authorized party for
multi-audience tokens, and RSA/EC/Ed25519 token signatures from the provider's
JWK set. Client secrets are encrypted at rest and can be retained by leaving
the secret field blank on a later metadata-only update, or explicitly removed
with **Clear stored OIDC client secret**.

Optional controls:

- Restrict sign-in/auto-provisioning to verified emails in specified domains.
- Keep auto-provisioning disabled to link only an existing administrator
  whose username exactly matches the provider's verified email address.
- Set a default role of `viewer`, `support`, or `admin`; superadministrator is
  never granted through OIDC.
- Map trusted provider groups to `viewer`, `support`, or `admin` with entries
  such as `engineering=admin, helpdesk=support`.

OIDC is intentionally optional. Local PHOENIX administrator credentials remain
available as a recovery path, and enabling/configuring OIDC requires recent
interactive superadministrator authentication.
