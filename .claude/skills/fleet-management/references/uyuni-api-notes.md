# Uyuni API quick reference (for fleet automation)

- Current release line: 2026.08 (rolling; check uyuni-project.org).
- API docs: https://www.uyuni-project.org/uyuni-docs-api/uyuni/index.html
- Two transports, same namespaces: legacy XML-RPC at `/rpc/api` and JSON
  over HTTP at `/rhn/manager/api/...`. Auth: `auth.login(user, pass)` →
  session token (cookie `pxt-session-cookie` for the HTTP API).

Namespaces you need for the vtessera fleet flow:

- `system` / `systemgroup` — create the "vtessera-nodes" group, list
  members, schedule actions.
- `activationkey` — bulk-create keys that auto-join the group and attach
  the config/formula on registration.
- `formula` — `setFormulasOfGroup`, `setGroupFormulaData` to push the
  vtessera-formula pillar (offer-index URL, pricing, device inventory).
- `configchannel` — override files like `/etc/vtessera/node.toml` per
  group.
- `recurringaction` — scheduled highstate / patch runs (central updates).
- `saltkey` — accept minion keys during bootstrap automation.

Bootstrap path: `mgr-bootstrap` generated script or
`system.bootstrapWithPrivateSshKey`; salt-minion connects to the Uyuni
server as master. Salt states live in an org channel or in
`/srv/salt`-style formula dirs packaged as
`packaging/salt/vtessera-formula/` in this repo.

Salt guide: https://www.uyuni-project.org/uyuni-docs/en/uyuni/specialized-guides/salt/salt-overview.html
