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
   once and expires in 15 minutes. Store it in a password manager or pass it
   directly to the node; do not put it in a shell history or a URL.
5. On the fleet node, run the agent over HTTPS:

   ```sh
   phoenix agent \
     --panel https://panel.example.com/panel-base \
     --bootstrap-token 'paste-the-one-time-token-here' \
     --state /etc/x-ui/phoenix-agent.json \
     --cloudflared-path /usr/local/bin/cloudflared
   ```

   The agent has no listener. It makes outbound check-ins, verifies normal TLS
   by default, and writes its credentials plus staged desired state atomically
   with mode `0600`. For a self-signed central panel, keep TLS verification on
   and add `--cert-sha256` with the central panel certificate's base64 SHA-256
   pin. `--allow-http` is only for a trusted development network. Omit
   `--cloudflared-path` if this node does not receive Cloudflare tunnels; the
   agent will report that provider as `unsupported` rather than executing an
   arbitrary program.
6. Confirm the enrollment becomes `online`. Create a one-agent pool (for
   example `fra-canary`) and optionally bind it to the template.
7. Deploy a specific revision to that pool. Inspect the enrollment
   `desiredRevision`, `appliedRevision`, `status`, and `lastError` before
   targeting the next pool. Creating a newer revision never deploys it.

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

Enroll once interactively with the bootstrap token, then run a long-lived unit
**without** that token. The state file contains the rotating credential; a
bootstrap token is one-use and must not be copied into a unit file.

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
ReadWritePaths=/etc/x-ui

[Install]
WantedBy=multi-user.target
```

Replace `/usr/local/bin/x-ui` with the installed PHOENIX executable. Before
starting the unit, verify that `cloudflared` is an operator-installed regular
executable at the exact absolute path and make the state/tunnel directory
owner-only (`0700`). Run `systemctl daemon-reload` followed by
`systemctl enable --now phoenix-fleet-agent`.

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
