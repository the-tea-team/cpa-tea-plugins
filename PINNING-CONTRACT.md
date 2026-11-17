# Optional member account/connection pins

Build capability requested by Kareem. No existing live combo pinned automatically. Native plugins only, CPA8.0.69; parent owns integration, docs, version, CI/PR.

## Data and routing contract

Member.accountId and Member.connectionId optional opaque strings, JSON+YAML. Trim whitespace on normalization. Mutually exclusive; optional provider remains optional; use target's provider if absent, reject mismatch at runtime. Pins require fresh existing quotaSnapshotPath inventory (no bare unchecked native IDs). Never guess IDs, infer from labels, fall back to AuthIndex or reconstruct missing IDs. Labels for UI only, stable opaque IDs stored.

EffectiveAccountStrategy: resolve member override/combo exactly as before, but a pin with resolved empty/inherit is explicitly round-robin within that target. Therefore pin overrides no-pin inherit; explicit quota-aware strategy stays quota-aware. No new strategy names. Existing members unchanged. No required accountPool for pins; if supplied it is an additional intersection, never ignored. quotaPool remains observation-only. Explicit policy without pin still requires accountPool as before.

Resolve fresh snapshot. accountId narrows to matching account and provider; connectionId narrows to exactly one credential ID and account/provider. If optional accountPool, intersect its membership/model/provider. Always enforce model exact advertised support, disabled/unavailable/nextRetry and provider consistency. Unknown/missing/stale/empty/exhausted target returns member-side503/429, no unpin or broaden to siblings. Even RR hard pins reject known-exhausted AccountScore. Core remains final authority and caller/context/header/model restrictions remain untouched.

Account pin retries only its eligible connections (existing max8), exact connection never rotates to siblings. Other combo members may fall back normally on member errors. Account/session rotation keys, cooldown identity, member-affinity identity and account-binding identity must include both pins (avoid same-model A failure poisoning B or edited pin inheriting old binding). Two same-model pin members with different targets must remain distinguishable. Legacy no-pin state remains readable; adding fields must invalidate old binding on edited target, not globally clear everything.

## Targets API/UI contract

GET /v0/management/plugins/tea-combos/targets authenticated via host management middleware. Static resource shell contains no inventory. Return allowlist projection from cached fresh snapshot:
{"accounts":[{"id","provider","label"}],"connections":[{"id","accountId","provider","label","models","disabled","unavailable"}]}
No raw auth-file/runtime callback response; no tokens, emails outside labels, attributes or metadata. Fail503 for unavailable/stale snapshot with bounded generic message. Display disabled/unavailable as such (they cannot route). Parent owns management endpoint+tests. AccountId is quota-account identity (may group multiple native credentials); connectionId is native credential ID, NOT authIndex.

UI two optional selects per member beside provider: account and connection, populated via authenticated targets API, filtered by selected provider and advertised model. Account options need supporting connection (also disabled visible marked). On choosing one clear the other only as explicit user action. Preserve saved unknown/unavailable selection as option rather than silently erase on edit/save; readback diff checks both fields. No manual secret entry required. Save no-op must preserve every existing field. Pin plus accountPool allowed as extra narrowing. UI help explains pin overrides inherit, unavailable target may fall through to next member but never another target. All untrusted text via textContent.

## Acceptance

Strict RED/GREEN tests; config decode/normalize/validation/persistence; account A with multiple credentials rejects B, exact connection rejects sibling; empty/stale/missing/wrong provider/model/disabled/cooldown/exhausted targets; optional pool intersection; concurrent same-model isolation; pin edit binding invalidation + independent cooldown; nil source errors fail closed. Nonstream/stream share existing plan path (no post-byte replay regression). Native CPA synthetic pin harness + existing four harnesses; browser targets selection save/readback incl mobile. Plugin version0.5.0. No production routing settings changed merely to demo feature.
