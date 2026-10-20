# Differential check: the YCLIENTS driver against the backend adapter

The YCLIENTS driver is the PadelApp backend's YCLIENTS adapter
(`backend/src/modules/admins/crm/integrations/yclients-crm/`, monorepo) moved into the gateway.
This check proves the move kept the rules: the same YCLIENTS answers go through both, and both
must read the same occupancy and send the same writes.

| file | what |
|---|---|
| `company-*`, `records-*`, `schedule-*` | live answers of branch 2270517 (06.10.2026), the backend adapter's own fixtures, no client data |
| `gen_scenarios.py` | builds `scenarios.json` from them plus synthetic cases |
| `scenarios.json` | 22 scenarios: occupancy (paging, holes in the schedule, closed and missing days, a record past midnight, a clock change, fail-closed answers), create (service tiling, repeated service → two records, an impossible length), cancel, paid mark |
| `yclients-difftest.spec.ts` | the TypeScript side: runs every scenario through `YclientsCrmService`, writes `ts-golden.json` |
| `ts-golden.json` | what the backend adapter read as occupied (per 30-minute slot) and every request it sent |
| `../../difftest_test.go` | the Go side: the same scenarios through the driver, compared with `ts-golden.json` |

What is compared: occupied 30-minute slots per court (the backend adapter rounds occupancy out to its 30-minute
grid; the gateway reports exact intervals, and a slot is occupied when one overlaps it), whether
the read fails, the booking id, and every write (`book_record`, `DELETE /record`, `PUT /record`)
plus the `book_services` read that decides what is booked — method, path, token kind (partner or
user), query and body.

Intended differences, not compared: the `book_record` comment (the backend adapter writes the player's data, the
gateway the partner's `[cg:<partner>]` marker and note — checked to start with the marker), and the
other reads (the gateway reads records from the day before the window, to see a record past
midnight, and reads each record before deleting it, to check ownership).

## Regenerating the golden file

In a monorepo checkout with `backend/node_modules`:

```
cp internal/driver/yclients/testdata/difftest/yclients-difftest.spec.ts <monorepo>/backend/src/modules/admins/crm/integrations/yclients-crm/
cd <monorepo>/backend
YCLIENTS_DIFF_SCENARIOS=<court-gateway>/internal/driver/yclients/testdata/difftest/scenarios.json \
YCLIENTS_DIFF_OUT=<court-gateway>/internal/driver/yclients/testdata/difftest/ts-golden.json \
npx jest src/modules/admins/crm/integrations/yclients-crm/yclients-difftest.spec.ts
```

The older variable names `D11_DIFF_SCENARIOS` and `D11_DIFF_OUT` still work; the new names win when both are set.

`ts-golden.json` here was written that way from monorepo `origin/stage` 391bd2d8 on 2026-10-17
(22 of 22 scenarios ran). A change of the backend adapter, or a new scenario, needs a new golden file.
