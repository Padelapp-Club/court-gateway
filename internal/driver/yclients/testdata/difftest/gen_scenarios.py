#!/usr/bin/env python3
"""Builds scenarios.json for the YCLIENTS differential check (see README.md).

The live fixtures (company-*, records-*, schedule-*) are the backend adapter's
own, copied from monorepo backend/src/modules/admins/crm/integrations/
yclients-crm/__fixtures__ (origin/stage 391bd2d8): answers of branch 2270517
on 2026-10-06, without client data. Everything else is synthetic.
"""
import json, os

here = os.path.dirname(os.path.abspath(__file__))
load = lambda n: json.load(open(os.path.join(here, n)))
company = load("company-2270517-r4.json")["data"]
records = load("records-2270517-2026-10-06-r4.json")
schedule = load("schedule-2270517-6121449-r4.json")["data"]

TZ = "Asia/Yekaterinburg"
C = "2270517"
LIVE_STAFF = ["6121440", "6121446", "6121434", "6121443", "6121449"]
A, B, COACH = "6121449", "6121440", "7000001"


def full(dates, frm="07:00", to="00:00"):
    return [{"date": d, "is_working": 1, "slots": [{"from": frm, "to": to}]} for d in dates]


def rec(i, staff, dt, length, deleted=False, comment=""):
    return {"id": i, "staff_id": int(staff), "datetime": dt, "length": length, "deleted": deleted, "comment": comment}


def occ(name, days, staff, pages, schedules, tz=TZ, card=None):
    return {"name": name, "kind": "occupancy", "tz": tz, "company": C, "days": days, "staff": staff,
            "pages": pages, "schedules": schedules, "card": card or {**company, "timezone_name": tz}}


D1, D2, D3 = "2026-10-06", "2026-10-07", "2026-10-08"
scenarios = [
    occ("live 2270517, one day", [D1], LIVE_STAFF, [records],
        {s: (schedule if s == A else full([D1])) for s in LIVE_STAFF}),
    occ("live 2270517, two days (schedule fixture)", [D1, D2], [A], [records], {A: schedule}),
    occ("a 16:05-17:05 record takes 16:00, 16:30 and 17:00", [D1], [A],
        [{"data": [rec(1, A, D1 + "T16:05:00+05:00", 3600)], "meta": {"page": 1, "total_count": 1}}], {A: full([D1])}),
    occ("deleted records and a coach's malformed one are not occupancy", [D1], [A],
        [{"data": [rec(1, A, D1 + "T10:00:00+05:00", 3600, deleted=True),
                   rec(2, COACH, "garbage", 3600)], "meta": {"page": 1, "total_count": 2}}], {A: full([D1])}),
    occ("a day not working and a day missing from schedule are occupied", [D1, D2, D3], [A],
        [{"data": [], "meta": {"page": 1, "total_count": 0}}],
        {A: full([D1]) + [{"date": D2, "is_working": 0, "slots": []}]}),
    occ("a record past midnight is cut at the club's midnight", [D1, D2], [A],
        [{"data": [rec(1, A, D1 + "T23:30:00+05:00", 3600)], "meta": {"page": 1, "total_count": 1}}],
        {A: full([D1, D2], "00:00", "00:00")}),
    occ("records over three pages", [D1], [A, B],
        [{"data": [rec(1, A, D1 + "T08:00:00+05:00", 3600), rec(2, B, D1 + "T09:00:00+05:00", 1800)], "meta": {"page": 1, "total_count": 5}},
         {"data": [rec(3, A, D1 + "T12:00:00+05:00", 5400), rec(4, B, D1 + "T13:00:00+05:00", 3600)], "meta": []},
         {"data": [rec(5, A, D1 + "T20:00:00+05:00", 7200)], "meta": {"page": 3, "total_count": 5}}],
        {A: full([D1]), B: full([D1])}),
    occ("an empty page before the promised total fails the read", [D1], [A],
        [{"data": [rec(1, A, D1 + "T08:00:00+05:00", 3600)], "meta": {"page": 1, "total_count": 3}},
         {"data": [], "meta": {"page": 2, "total_count": 3}}], {A: full([D1])}),
    occ("an unreadable record on a court fails the read", [D1], [A],
        [{"data": [rec(1, A, "2026-10-06 16:00", 3600)], "meta": {"page": 1, "total_count": 1}}], {A: full([D1])}),
    occ("a schedule that is not a list fails the read", [D1], [A],
        [{"data": [], "meta": {"page": 1, "total_count": 0}}], {A: {"date": D1}}),
    occ("a booking of two records reads as the same slots", [D1], [A],
        [{"data": [rec(1, A, D1 + "T18:00:00+05:00", 3600, comment="[cg:padelapp] [cg-set:0a1b2c3d/2] x"),
                   rec(2, A, D1 + "T19:00:00+05:00", 3600, comment="[cg:padelapp] [cg-set:0a1b2c3d/2] x")],
          "meta": {"page": 1, "total_count": 2}}], {A: full([D1])}),
    occ("days across a clock change (Europe/Berlin, 25.10)", ["2026-10-24", "2026-10-25", "2026-10-26"], [A],
        [{"data": [rec(1, A, "2026-10-25T02:30:00+01:00", 3600), rec(2, A, "2026-10-25T01:30:00+02:00", 1800)],
          "meta": {"page": 1, "total_count": 2}}],
        {A: [{"date": "2026-10-24", "is_working": 1, "slots": [{"from": "07:00", "to": "00:00"}]},
             {"date": "2026-10-25", "is_working": 1, "slots": [{"from": "00:00", "to": "12:00"}, {"from": "14:00", "to": "00:00"}]},
             {"date": "2026-10-26", "is_working": 1, "slots": [{"from": "07:00", "to": "23:00"}]}]},
        tz="Europe/Berlin"),
]

S1H = {"id": 101, "title": "1 ч", "seance_length": 3600, "price_min": 1000, "active": 1}
S90 = {"id": 102, "title": "1,5 ч", "seance_length": 5400, "price_min": 1400, "active": 1}
S2H = {"id": 103, "title": "2 ч", "seance_length": 7200, "price_min": 1800, "active": 1}
S30 = {"id": 104, "title": "30 мин", "seance_length": 1800, "price_min": 600, "active": 1}


def create(name, start, end, services, n):
    return {"name": name, "kind": "create", "tz": TZ, "company": C, "staff": A, "start": start, "end": end,
            "services": services, "record_ids": [2017335528 + i * 6 for i in range(n)]}


scenarios += [
    create("1.5 h: one service", D2 + "T10:00:00+05:00", D2 + "T11:30:00+05:00", [S1H, S90, S2H], 1),
    create("2 h without a 2 h service: 1 h + 1 h, two records", D2 + "T10:00:00+05:00", D2 + "T12:00:00+05:00", [S1H, S90], 2),
    create("2.5 h: 1.5 h and 1 h in one record", D2 + "T18:00:00+05:00", D2 + "T20:30:00+05:00", [S1H, S90], 1),
    create("3 h: 1.5 h twice, two records", D2 + "T09:00:00+05:00", D2 + "T12:00:00+05:00", [S1H, S90], 2),
    create("cheaper of two services of one length", D2 + "T10:00:00+05:00", D2 + "T11:00:00+05:00", [S1H, {**S1H, "id": 201, "price_min": 500}], 1),
    create("a length no services make: nothing is booked", D2 + "T10:00:00+05:00", D2 + "T10:45:00+05:00", [S1H, S90], 0),
    create("30 min over midnight-free evening", D2 + "T23:30:00+05:00", D3 + "T00:00:00+05:00", [S30, S1H], 1),
]

PHONE = "79000000000"


def detail(i, start, length, comment, attendance=0):
    return {"id": i, "staff_id": int(A), "datetime": start, "seance_length": length, "length": length, "deleted": False,
            "comment": comment, "attendance": attendance,
            "services": [{"id": 101, "title": "1 ч", "cost": 1000, "first_cost": 1000, "discount": 0, "amount": 1}],
            "client": {"name": "PadelApp", "phone": "+7900XXXXX00"}}


scenarios += [
    {"name": "cancel a booking of two records", "kind": "cancel", "tz": TZ, "company": C, "staff": A,
     "records": [detail(2017335528, D2 + "T10:00:00+05:00", 3600, "[cg:padelapp] [cg-set:0a1b2c3d/2] x"),
                 detail(2017335534, D2 + "T11:00:00+05:00", 3600, "[cg:padelapp] [cg-set:0a1b2c3d/2] x")]},
    {"name": "mark a booking paid", "kind": "paid", "tz": TZ, "company": C, "staff": A,
     "records": [detail(2017335528, D2 + "T10:00:00+05:00", 3600, "[cg:padelapp] Комментарий", attendance=1)]},
    {"name": "mark a booking of two records paid", "kind": "paid", "tz": TZ, "company": C, "staff": A,
     "records": [detail(2017335528, D2 + "T10:00:00+05:00", 3600, "[cg:padelapp] [cg-set:0a1b2c3d/2]"),
                 detail(2017335534, D2 + "T11:00:00+05:00", 3600, "[cg:padelapp] [cg-set:0a1b2c3d/2]")]},
]

json.dump({"support_phone": PHONE, "support_email": "info@padelapp.club", "scenarios": scenarios},
          open(os.path.join(here, "scenarios.json"), "w"), ensure_ascii=False, indent=1)
print(len(scenarios), "scenarios")
