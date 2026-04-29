# Proposal: mdadm Tiering Infrastructure — GET /api/tiers/{name} (Single Pool Detail)

**Status:** Pending
**Part of:** mdadm-tiering-infrastructure (Step 9 of 14)
**Depends on:** mdadm-tiering-infra-08-list-pools

---

## Problem

The list endpoint (Step 8) returns all pools together, which is inefficient when the UI or a polling loop only needs to track the state of one pool — for example, during provisioning or after an array assignment.

---

## Specification

### Request

`GET /api/tiers/{name}`

### Response

**200 OK** — a single pool object using the same schema as one element from `GET /api/tiers` (Step 8).

**404 Not Found** — if no pool with `{name}` exists:
```json
{ "error": "pool not found" }
```

---

## Acceptance Criteria

- [ ] Returns the named pool with full tier slot detail and live capacity figures.
- [ ] Returns `404` for unknown pool names.
- [ ] Response schema is identical to a single element from `GET /api/tiers`.
