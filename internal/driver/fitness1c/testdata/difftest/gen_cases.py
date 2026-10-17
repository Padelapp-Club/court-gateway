#!/usr/bin/env python3
"""Builds cases.json for the differential check of the 1C grid rules.

Every case is one input both implementations read: the backend adapter
(TypeScript, fitness1c-crm.service.ts) through ts/fitness1c-difftest.spec.ts,
the gateway driver (Go) through ../../difftest_test.go. The bodies are D14's
fixtures (fixtures/, ids replaced by fake ones) and variations of them that
hit one rule each. Run: python3 gen_cases.py > cases.json
"""
import copy
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def fixture(name):
    with open(os.path.join(HERE, "fixtures", name), encoding="utf-8") as f:
        return json.load(f)["data"]


day06 = fixture("rental_rooms_times-2026-09-06.json")
day08 = fixture("rental_rooms_times-2026-09-08-duplicate-slot.json")
MSK = "Europe/Moscow"


def slot(dt, busy, **extra):
    s = {"time": dt[11:], "seance_length": 30, "date_time": dt, "rental_id": None, "price": None,
         "service_title": None, "is_busy": busy}
    s.update(extra)
    return s


cases = []


def case(name, days, per_day, tz=MSK, slots=None):
    cases.append({"name": name, "timeZone": tz, "days": days, "perDay": per_day, "slots": slots or []})


# probes of our own slots, as local "YYYY-MM-DD HH:MM" pairs: aligned,
# unaligned, outside 1C's hours, the day's edges
probes06 = [["2026-09-06 07:15", "2026-09-06 07:45"], ["2026-09-06 06:30", "2026-09-06 07:30"],
            ["2026-09-06 22:30", "2026-09-06 23:30"], ["2026-09-06 23:30", "2026-09-07 00:00"],
            ["2026-09-06 10:00", "2026-09-06 11:30"], ["2026-09-06 00:00", "2026-09-06 00:30"]]

# 1. the live bodies as they are
case("fixture 2026-09-06", ["2026-09-06"], [day06], slots=probes06)
case("fixture 2026-09-08 duplicate slot", ["2026-09-08"], [day08],
     slots=[["2026-09-08 07:00", "2026-09-08 07:30"], ["2026-09-08 07:15", "2026-09-08 08:15"]])

# 2. two days in one window
case("two days", ["2026-09-06", "2026-09-08"], [day06, day08], slots=probes06)

# 3. a room repeated three times in a day's answer (1C's form for a range)
case("room repeated", ["2026-09-06"], [day06 + copy.deepcopy(day06) + copy.deepcopy(day06)])

# 4. another day's slot inside a day's answer is not that day's
other_day = copy.deepcopy(day06)
other_day[1]["durations"].append(slot("2026-09-07 08:00", False))
other_day[1]["durations"].append(slot("2026-09-05 23:30", False))
case("another day's slots", ["2026-09-06"], [other_day])

# ... and observable: the next day is in the window. Its own answer lists
# 09:00 free and nothing at 08:00; the earlier day's answer says the
# opposite, and must not be believed
next_day = [{"room": r["room"], "durations": [slot("2026-09-07 09:00", False)]} for r in day06]
leak = copy.deepcopy(day06)
leak[1]["durations"].append(slot("2026-09-07 08:00", False))
leak[1]["durations"].append(slot("2026-09-07 09:00", True))
case("another day's slots, that day in the window", ["2026-09-06", "2026-09-07"], [leak, next_day])

# 5. the same half-hour listed free and busy: busy
conflict = copy.deepcopy(day06)
first = conflict[0]["durations"][4]
conflict[0]["durations"].append(dict(first, is_busy=not first["is_busy"]))
case("free and busy at once", ["2026-09-06"], [conflict])

# 6. is_busy (a temporary reservation) is busy
flagged = copy.deepcopy(day06)
for s in flagged[2]["durations"][3:6]:
    s["is_busy"] = True
    s["rental_id"] = "00000000-0000-4000-8000-00000000e002"
case("is_busy slots", ["2026-09-06"], [flagged])

# 7. a day with no slot at all: the whole day busy
empty = [{"room": r["room"], "durations": []} for r in day06]
case("empty day", ["2026-09-06"], [empty], slots=probes06)

# 8. a half-hour missing on every room (a daily technical hour)
tech = copy.deepcopy(day06)
for r in tech:
    r["durations"] = [s for s in r["durations"] if s["time"] != "14:00"]
case("technical hour on every room", ["2026-09-06"], [tech])

# 9. a room missing from one of two days
partial = [r for r in copy.deepcopy(day08) if r["room"]["id"] != day08[3]["room"]["id"]]
case("room missing on a day", ["2026-09-06", "2026-09-08"], [day06, partial])

# 10. slots off the grid (07:15): never a key of the grid
off = copy.deepcopy(day06)
off[0]["durations"].append(slot("2026-09-06 07:15", False))
case("off-grid slot", ["2026-09-06"], [off], slots=[["2026-09-06 07:15", "2026-09-06 07:45"]])

# 11. another zone: the same body read in Asia/Vladivostok
case("other zone", ["2026-09-06"], [day06], tz="Asia/Vladivostok", slots=probes06)

# 12. bodies that cannot be trusted: refused whole
for label, mutate in [
    ("no is_busy", lambda s: s.pop("is_busy")),
    ("is_busy null", lambda s: s.update(is_busy=None)),
    ("is_busy a string", lambda s: s.update(is_busy="false")),
    ("date_time with T", lambda s: s.update(date_time=s["date_time"].replace(" ", "T"))),
    ("date_time with seconds", lambda s: s.update(date_time=s["date_time"] + ":00")),
    ("time without leading zero", lambda s: s.update(time="7:00")),
    ("no time", lambda s: s.pop("time")),
    ("date_time a number", lambda s: s.update(date_time=20260906)),
]:
    bad = copy.deepcopy(day06)
    mutate(bad[2]["durations"][7])
    case("invalid: " + label, ["2026-09-06"], [bad])

# days of a window (daysBetween): from, to as instants
windows = [
    {"from": "2026-09-05T21:00:00Z", "to": "2026-09-19T21:00:00Z", "timeZone": MSK},
    {"from": "2026-09-06T20:59:00Z", "to": "2026-09-06T21:00:00Z", "timeZone": MSK},
    {"from": "2026-09-06T10:00:00Z", "to": "2026-09-06T11:00:00Z", "timeZone": MSK},
    {"from": "2026-09-06T13:59:00Z", "to": "2026-09-06T14:01:00Z", "timeZone": "Asia/Vladivostok"},
    {"from": "2026-10-24T22:00:00Z", "to": "2026-10-26T02:00:00Z", "timeZone": "Europe/Berlin"},
]

json.dump({"cases": cases, "windows": windows}, sys.stdout, ensure_ascii=False, indent=1)
sys.stdout.write("\n")
