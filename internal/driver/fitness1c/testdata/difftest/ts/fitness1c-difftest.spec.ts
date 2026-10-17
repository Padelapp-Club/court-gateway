/**
 * The TypeScript side of the differential check of the 1C grid rules
 * (court-gateway internal/driver/fitness1c/testdata/difftest). It is not a
 * test of the monorepo: copy it next to fitness1c-crm.service.ts in a
 * scratch checkout of the monorepo and run
 *
 *   DIFFTEST_CASES=<cases.json> DIFFTEST_OUT=<ts-golden.json> \
 *     npx jest src/modules/admins/crm/integrations/fitness1c-crm/fitness1c-difftest.spec.ts
 *
 * It answers every case of cases.json with the adapter's own functions —
 * normalizeGrid, getBreaksForSlots, isIntervalExplicitlyFree, daysBetween —
 * and writes what they said; the Go test compares the driver's answers
 * to that file.
 */
import { readFileSync, writeFileSync } from 'fs';

import { DateTime } from 'luxon';

import { Fitness1cCrmService } from './fitness1c-crm.service';
import { IFitness1cRoomSlots } from './interfaces/fitness1c-crm.interface';

interface Case {
  name: string;
  timeZone: string;
  days: string[];
  perDay: IFitness1cRoomSlots[][];
  slots: [string, string][];
}

const SERVICE = { id: 'difftest-service', seance_length: 30 };
const proto = Fitness1cCrmService.prototype;

function halves(day: string, zone: string): { key: string; fromMs: number; toMs: number }[] {
  const out: { key: string; fromMs: number; toMs: number }[] = [];
  const start = DateTime.fromISO(day, { zone }).startOf('day');
  const next = start.plus({ days: 1 });
  for (let t = start; t < next; t = t.plus({ minutes: 30 })) {
    out.push({ key: t.toFormat('HH:mm'), fromMs: t.toMillis(), toMs: t.plus({ minutes: 30 }).toMillis() });
  }
  return out;
}

it('answers the differential cases', () => {
  const input = JSON.parse(readFileSync(process.env.DIFFTEST_CASES as string, 'utf8'));
  const cases = (input.cases as Case[]).map((c) => {
    let grid;
    try {
      grid = Fitness1cCrmService.normalizeGrid('difftest-club', c.timeZone, SERVICE, c.days, c.perDay);
    } catch {
      return { name: c.name, error: true };
    }
    const rooms: Record<string, { busy: Record<string, string[]>; probes: { busy: boolean; explicitFree: boolean }[] }> = {};
    for (const roomId of [...grid.rooms.keys()].sort()) {
      const busy: Record<string, string[]> = {};
      for (const day of c.days) {
        busy[day] = halves(day, c.timeZone)
          .filter((h) => {
            const isBusy = proto.getBreaksForSlots.call(null, roomId, grid, [{ fromMs: h.fromMs, toMs: h.toMs }]).length > 0;
            const free = proto.isIntervalExplicitlyFree.call(null, roomId, grid, h.fromMs, h.toMs);
            if (isBusy === free) throw new Error(`${c.name}: ${roomId} ${day} ${h.key} busy and free disagree`);
            return isBusy;
          })
          .map((h) => h.key);
      }
      const probes = c.slots.map(([from, to]) => {
        const fromMs = DateTime.fromFormat(from, 'yyyy-MM-dd HH:mm', { zone: c.timeZone }).toMillis();
        const toMs = DateTime.fromFormat(to, 'yyyy-MM-dd HH:mm', { zone: c.timeZone }).toMillis();
        return {
          busy: proto.getBreaksForSlots.call(null, roomId, grid, [{ fromMs, toMs }]).length > 0,
          explicitFree: proto.isIntervalExplicitlyFree.call(null, roomId, grid, fromMs, toMs),
        };
      });
      rooms[roomId] = { busy, probes };
    }
    return { name: c.name, error: false, rooms };
  });
  const windows = (input.windows as { from: string; to: string; timeZone: string }[]).map((w) =>
    Fitness1cCrmService.daysBetween(new Date(w.from), new Date(w.to), w.timeZone),
  );
  writeFileSync(process.env.DIFFTEST_OUT as string, JSON.stringify({ cases, windows }, null, 1) + '\n');
});
