/**
 * D11 differential check, TypeScript side. Not part of the monorepo: copy it into
 * backend/src/modules/admins/crm/integrations/yclients-crm/ of a monorepo checkout and run
 *
 *   D11_DIFF_SCENARIOS=<court-gateway>/internal/driver/yclients/testdata/d11diff/scenarios.json \
 *   D11_DIFF_OUT=<court-gateway>/internal/driver/yclients/testdata/d11diff/ts-golden.json \
 *   npx jest src/modules/admins/crm/integrations/yclients-crm/d11-diff.spec.ts
 *
 * Every scenario goes through the D11 adapter (YclientsCrmService) with its HTTP client replaced
 * by the scenario's answers; what it reads as occupied (per 30-minute slot) and every request it
 * sends are written to D11_DIFF_OUT. The Go test (d11diff_test.go) runs the same scenarios through
 * the gateway driver and compares.
 */
import { Logger } from '@nestjs/common';

import { readFileSync, writeFileSync } from 'fs';
import { DateTime } from 'luxon';

import { YclientsCrmService } from './yclients-crm.service';

interface Req {
  method: string;
  path: string;
  auth: string;
  query?: Record<string, string | number>;
  body?: unknown;
}

const input = JSON.parse(readFileSync(process.env.D11_DIFF_SCENARIOS as string, 'utf8'));
const out: Record<string, unknown> = {};

const slotsOf = (days: string[], tz: string): { fromMs: number; toMs: number }[] => {
  const start = DateTime.fromISO(days[0], { zone: tz }).startOf('day');
  const end = DateTime.fromISO(days[days.length - 1], { zone: tz }).plus({ days: 1 }).startOf('day');
  const slots: { fromMs: number; toMs: number }[] = [];
  for (let t = start.toMillis(); t < end.toMillis(); t += 30 * 60_000) slots.push({ fromMs: t, toMs: t + 30 * 60_000 });
  return slots;
};

const utc = (ms: number) => DateTime.fromMillis(ms, { zone: 'utc' }).toISO({ suppressMilliseconds: true });

describe('D11 differential scenarios', () => {
  beforeAll(() => {
    jest.spyOn(Logger.prototype, 'warn').mockImplementation(() => undefined);
    jest.spyOn(Logger.prototype, 'log').mockImplementation(() => undefined);
    process.env.SUPPORT_PHONE = input.support_phone;
    process.env.SUPPORT_EMAIL = input.support_email;
  });

  afterAll(() => {
    writeFileSync(process.env.D11_DIFF_OUT as string, JSON.stringify(out, null, 1) + '\n');
  });

  for (const sc of input.scenarios) {
    it(sc.name, async () => {
      const requests: Req[] = [];
      const service = new YclientsCrmService({} as never, {} as never);
      const records = new Map<string, Record<string, unknown>>(
        (sc.records ?? []).map((r: Record<string, unknown>) => [String(r.id), { ...r }]),
      );
      const request = async (req: Req) => {
        requests.push(JSON.parse(JSON.stringify(req)));
        if (req.path === `/company/${sc.company}/`) return sc.card;
        const sched = req.path.match(/^\/schedule\/\d+\/(\d+)\//);
        if (sched) return sc.schedules[sched[1]];
        if (req.path === `/book_services/${sc.company}`) return { services: sc.services };
        if (req.path === `/book_record/${sc.company}`) {
          return sc.record_ids.map((id: number, i: number) => ({ id: i + 1, record_id: id, record_hash: 'h' }));
        }
        const rec = req.path.match(/^\/record\/\d+\/(\d+)$/);
        if (rec) {
          const r = records.get(rec[1]);
          if (req.method === 'PUT' && r) r.comment = (req.body as { comment: string }).comment;
          if (req.method === 'DELETE') return undefined;
          return r ? JSON.parse(JSON.stringify(r)) : undefined;
        }
        throw new Error(`unexpected request ${req.method} ${req.path}`);
      };
      const requestPage = async (req: Req) => {
        requests.push(JSON.parse(JSON.stringify(req)));
        const page = sc.pages[Number(req.query?.page) - 1] ?? { data: [], meta: [] };
        return { data: page.data, meta: page.meta };
      };
      (service as unknown as { api: unknown }).api = { request, requestPage };

      const result: Record<string, unknown> = {};
      try {
        if (sc.kind === 'occupancy') {
          const from = DateTime.fromISO(`${sc.days[0]}T00:30`, { zone: sc.tz }).toJSDate();
          const to = DateTime.fromISO(`${sc.days[sc.days.length - 1]}T23:00`, { zone: sc.tz }).toJSDate();
          const grid = await service.getOccupancyGrid('club', sc.company, sc.tz, sc.staff, from, to);
          const busy: Record<string, string[]> = {};
          for (const staff of sc.staff) {
            busy[staff] = service
              .getBreaksForSlots(staff, grid, slotsOf(sc.days, sc.tz))
              .map((b) => utc(b.timeFrom.getTime()) as string);
          }
          result.busy = busy;
        } else if (sc.kind === 'create') {
          const created = await service.createBooking(
            'club',
            {
              fullname: 'Игрок',
              clientEmail: 'player@example.test',
              staffId: sc.staff,
              companyId: sc.company,
              timeFrom: new Date(sc.start),
              timeTo: new Date(sc.end),
              description: 'note',
            },
            sc.tz,
          );
          result.id = created.id;
        } else if (sc.kind === 'cancel') {
          await service.deleteBooking('club', sc.records.map((r: { id: number }) => r.id).join(','), sc.company);
        } else if (sc.kind === 'paid') {
          await service.markBookingAsPaid('club', sc.records.map((r: { id: number }) => r.id).join(','), sc.company);
        }
      } catch (error) {
        result.error = (error as Error).message;
      }
      result.requests = requests;
      out[sc.name] = result;
    });
  }
});
