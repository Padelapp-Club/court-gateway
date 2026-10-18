# Differential check: the 1C grid rules against the backend adapter

The driver's grid rules are the backend adapter's (D14,
`backend/src/modules/admins/crm/integrations/fitness1c-crm/fitness1c-crm.service.ts`
in the monorepo). This directory proves it on the same bodies:

- `fixtures/` — D14's `__fixtures__` (live answers of `rental_rooms_times`), room and
  rental ids replaced by fake ones (this repository is public);
- `gen_cases.py` → `cases.json` — those bodies and variations that each hit one rule
  (repeated rooms, another day's slots, free and busy at once, `is_busy`, an empty day,
  a technical hour, a missing room, off-grid slots, another zone, eight untrusted
  bodies) plus windows for the days of a range;
- `ts/fitness1c-difftest.spec.ts` — answers every case with the adapter's own
  `normalizeGrid`, `getBreaksForSlots`, `isIntervalExplicitlyFree`, `daysBetween`;
- `ts-golden.json` — what it answered; `ts-golden.source` names the monorepo commit;
- `../../difftest_test.go` — the driver's answers compared to `ts-golden.json`.

Regenerate after a change of the cases or of the adapter:

```sh
python3 gen_cases.py > cases.json
# in a scratch checkout of the monorepo with backend/node_modules:
cp ts/fitness1c-difftest.spec.ts <monorepo>/backend/src/modules/admins/crm/integrations/fitness1c-crm/
cd <monorepo>/backend && DIFFTEST_CASES=$PWD_OF_THIS_DIR/cases.json DIFFTEST_OUT=$PWD_OF_THIS_DIR/ts-golden.json \
  npx jest src/modules/admins/crm/integrations/fitness1c-crm/fitness1c-difftest.spec.ts
```

What it does not cover: writes (create, cancel, compensation) — the adapter's are
HTTP calls whose rules are tested in `../../driver_test.go` against the stub.
