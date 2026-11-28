# Phase C fleet operations

Phase C is opt-in. Upgrading PHOENIX creates no agent, pool, fleet template,
or managed tunnel and does not alter legacy node polling or existing inbounds.
See [phase-c-fleet-design.md](phase-c-fleet-design.md) for the protocol and
migration contract.

## Safe first rollout

1. Sign in as a superadmin and complete recent authentication.
2. Create a managed tunnel endpoint only if needed. Its target must be
   `localhost`, a `.localhost` name, or a private/loopback IP. The provider
   configuration is write-only after it is saved.
3. Create a fleet template with `schemaVersion: 1`. Start with a simple
   `node.inboundSyncMode: "all"` policy; use `selected` only with a reviewed
   unique tag list. A template revision is immutable.
4. Create an enrollment. The returned `bootstrapToken` is displayed exactly
   once and expires in 15 minutes. To install the agent from the Fleet control
   plane, call `POST /panel/api/fleet/enrollments/:id/installer` with the
   exact HTTPS `panelUrl` (and an optional `certSha256` pin). This invalidates
   the earlier unused token and returns a fresh, short-lived shell script.
5. Review the generated script and run it **locally as root on the fleet
   node**. It downloads only the latest official PHOENIX release, verifies the
   matching published SHA-256 file before extraction, enrolls once, and then
   installs `phoenix-fleet-agent.service` without leaving the bootstrap token
   in the unit. The panel does not connect to the node, retain SSH keys, or
   execute the script. For a self-signed central panel, keep TLS verification
   on and include its base64 certificate SHA-256 as `certSha256`. The installer
   always requires HTTPS and has no custom repository, binary, or HTTP option.

   If PHOENIX is already installed and you prefer the manual path, run:

   ```sh
   phoenix agent \
     --panel https://panel.example.com/panel-base \
     --bootstrap-token 'paste-the-one-time-token-here' \
     --state /etc/x-ui/phoenix-agent.json \
     --cloudflared-path /usr/local/bin/cloudflared
   ```

   The agent has no listener. It makes outbound check-ins and writes its
   credentials plus staged desired state atomically with mode `0600`. Omit
   `--cloudflared-path` if this node does not receive Cloudflare tunnels; the
   agent will report that provider as `unsupported` rather than executing an
   arbitrary program. The generated installer also sets a fixed local
   `--update-arch` enum. A manually provisioned ARM agent must use the exact
   installer-selected `armv5`, `armv6`, or `armv7` value to opt into official
   updates; the panel never supplies this value.
6. Confirm the enrollment becomes `online`. Create a one-agent pool (for
   example `fra-canary`) and optionally bind it to the template.
7. Deploy a specific revision to that pool. Inspect the enrollment
   `desiredRevision`, `appliedRevision`, `status`, and `lastError` before
   targeting the next pool. Creating a newer revision never deploys it.
8. To update a Fleet node, select one canary enrollment and call
   `POST /panel/api/fleet/enrollments/:id/update` with an empty body. The
   panel records a numeric official release tag plus the matching
   architecture-specific archive SHA-256 at queue time. The acknowledgement
   deadline is 30 minutes. Wait for its `update` status to become `installed`
   or `failed` before selecting the next explicit
   cohort. A failure leaves the existing binary running; resolve the bounded
   error and issue a new explicit update request. There is no release URL,
   version, binary, argument, environment, upload, or rollback-artifact
   parameter.

The agent never runs a command received from the panel. It has a built-in
supervised `cloudflared` adapter for `cloudflared` tunnel contracts, but the
executable path is supplied locally through `--cloudflared-path` and must be an
absolute executable file. The panel only supplies a validated endpoint and
credential contract; it cannot choose a binary, arguments, shell, or
environment. The agent writes generated Cloudflare credential/config files
under `--tunnel-dir` with owner-only permissions and reports bounded process
health on its next check-in. Other providers remain safely contract-only until
their dedicated adapter is installed.

## Run the agent under systemd

Enroll once with `phoenix agent --once` and the bootstrap token, then run a
long-lived unit **without** that token. The state file contains the rotating
credential; a bootstrap token is one-use and must not be copied into a unit
file.

```ini
# /etc/systemd/system/phoenix-fleet-agent.service
[Unit]
Description=PHOENIX Fleet Node Agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
# Use the account that owns /etc/x-ui and has permission to execute cloudflared.
User=root
ExecStart=/usr/local/bin/x-ui agent --panel https://panel.example.com/panel-base --state /etc/x-ui/phoenix-agent.json --tunnel-dir /etc/x-ui/managed-tunnels --cloudflared-path /usr/local/bin/cloudflared
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
# Atomic rename needs the parent directory; the agent hardcodes x-ui as target.
ReadWritePaths=/etc/x-ui /usr/local/bin

[Install]
WantedBy=multi-user.target
```

Keep the installer-owned `/usr/local/bin/x-ui` path for Fleet
official-release updates; an agent started from another executable remains
safe but does not advertise update capability. Before starting the unit,
verify that `cloudflared` is an operator-installed regular executable at the
exact absolute path and make the state/tunnel directory owner-only (`0700`).
Run `systemctl daemon-reload` followed by `systemctl enable --now
phoenix-fleet-agent`.

A reachable disabled enrollment receives a terminal response: the agent stops
all managed connectors and removes their generated files, but preserves its
protected desired-state record so re-enabling can resume the assigned template.
An offline node cannot be stopped by an outbound-only protocol; stop its local
agent service for immediate isolation. Disabling does not alter a legacy node,
inbound, client, or traffic record.

## Credential recovery and rollback

* Rotating an enrolled credential returns a replacement once. Update the
  agent's protected state before its next check-in. If an agent loses both its
  state and credential, create a fresh enrollment; bootstrap values cannot be
  recovered from the panel.
* To roll back, deploy a previous immutable revision to the affected explicit
  enrollment list or pool. Do not edit history.
* Disabling an enrollment prevents normal desired-state delivery. A reachable
  agent receives the terminal response on its next poll, stops managed
  connectors, and removes generated connector files; its protected staged state
  and all legacy node, inbound, client, and traffic data remain intact.
* Referenced tunnel records cannot be deleted because history must stay
  reproducible. Disable the tunnel or publish a new template revision that no
  longer references it.

## Legacy-node compatibility

An enrollment may optionally link an existing node ID for inventory context.
That link does not replace the node URL/API-token transport, traffic snapshots,
remote runtime, or heartbeat jobs. Continue using the existing Nodes interface
for direct-panel operations while adopting Phase C gradually.
