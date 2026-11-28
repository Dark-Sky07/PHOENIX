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
     --state /etc/x-ui/phoenix-agent.json
   ```

   The agent has no listener. It makes outbound check-ins, verifies normal TLS
   by default, and writes its credentials plus staged desired state atomically
   with mode `0600`. For a self-signed central panel, keep TLS verification on
   and add `--cert-sha256` with the central panel certificate's base64 SHA-256
   pin. `--allow-http` is only for a trusted development network.
6. Confirm the enrollment becomes `online`. Create a one-agent pool (for
   example `fra-canary`) and optionally bind it to the template.
7. Deploy a specific revision to that pool. Inspect the enrollment
   `desiredRevision`, `appliedRevision`, `status`, and `lastError` before
   targeting the next pool. Creating a newer revision never deploys it.

The current agent is a safe **stage-only** control-plane component: it validates
and stores the template and managed-tunnel contracts but never runs a command
received from the panel. Provider-specific connector adapters consume the
staged tunnel contract independently. This is deliberate: a template editor
cannot turn into arbitrary command execution on a node.

## Credential recovery and rollback

* Rotating an enrolled credential returns a replacement once. Update the
  agent's protected state before its next check-in. If an agent loses both its
  state and credential, create a fresh enrollment; bootstrap values cannot be
  recovered from the panel.
* To roll back, deploy a previous immutable revision to the affected explicit
  enrollment list or pool. Do not edit history.
* Disabling an enrollment stops desired-state delivery immediately. It does
  not delete the node's last staged state, tunnel contract, legacy node record,
  inbound, client, or traffic data.
* Referenced tunnel records cannot be deleted because history must stay
  reproducible. Disable the tunnel or publish a new template revision that no
  longer references it.

## Legacy-node compatibility

An enrollment may optionally link an existing node ID for inventory context.
That link does not replace the node URL/API-token transport, traffic snapshots,
remote runtime, or heartbeat jobs. Continue using the existing Nodes interface
for direct-panel operations while adopting Phase C gradually.
