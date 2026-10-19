package pair

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

type action struct {
	side int
	item driver.InventoryItem
}

func overlap(a, b driver.Interval) bool {
	return a.Court == b.Court && a.Start.Before(b.End) && b.Start.Before(a.End)
}
func valid(m, s driver.InventoryItem, source string) bool {
	return s.Class == driver.ClubBooking && driver.SameInterval(m.Interval, s.Interval) && (m.Source == "" && m.SourceID == "" || m.Source == source && m.SourceID == s.ID)
}
func evidence(side int, it driver.InventoryItem) string {
	return fmt.Sprintf("%d/%s/%s/%s/%s/%s/%s", side, it.ID, it.Court, it.Start.UTC().Format(time.RFC3339Nano), it.End.UTC().Format(time.RFC3339Nano), it.Source, it.SourceID)
}

// Reconcile plans from two complete snapshots before doing any write.
// The process-wide court locks protect each write and its fresh checks.
func (d *Driver) Reconcile(ctx context.Context) (result error) {
	d.pass.Lock()
	defer d.pass.Unlock()
	planned := false
	defer func() {
		if !planned {
			d.absent = map[string]bool{}
			d.halves = map[string]bool{}
			d.capped = 0
		}
		d.mu.Lock()
		d.alarm = result
		d.mu.Unlock()
		if result != nil {
			d.log.Warn("Сверка CRM: требуется внимание клуба и оператора", "error", result)
		}
	}()
	now, known := d.clock()
	if !known {
		return driver.ErrClockUnknown
	}
	h, e := d.horizon(ctx)
	if e != nil {
		return e
	}
	q := driver.OccupancyQuery{Courts: d.courts, From: now, To: now.Add(h)}
	ms := [2]driver.Mirrorer{d.ma, d.mb}
	ds := [2]driver.Driver{d.a, d.b}
	types := [2]string{d.typ, "lunda"}
	var inv [2]driver.Inventory
	for i := range inv {
		inv[i], e = ms[i].Inventory(ctx, q)
		if e != nil || !inv[i].Complete {
			d.absent = map[string]bool{}
			d.halves = map[string]bool{}
			return alert("MIRROR_INCOMPLETE", "Сверка прервана: не удалось полностью прочитать обе CRM. Ничего не записано.")
		}
	}
	for _, a := range inv[0].Items {
		for _, b := range inv[1].Items {
			if a.Class == driver.ClubBooking && b.Class == driver.ClubBooking && overlap(a.Interval, b.Interval) {
				d.absent = map[string]bool{}
				return alert("MIRROR_CONFLICT", "Брони клуба пересекаются в двух CRM. Клубу и оператору: устраните пересечение вручную; ничего не изменено.")
			}
		}
	}
	planned = true
	var alarms []error
	var creates, deletes []action
	absent, halves := map[string]bool{}, map[string]bool{}
	mapped := map[string]bool{}
	for _, c := range d.courts {
		mapped[c] = true
	}
	for side := range inv {
		other := 1 - side
		for _, it := range inv[side].Items {
			if !mapped[it.Court] {
				if it.Class == driver.MirrorBooking {
					alarms = append(alarms, alert("MIRROR_UNMAPPED", "Отражение вне сопоставления кортов: оставлено без изменений."))
				}
				continue
			}
			switch it.Class {
			case driver.DamagedMirror:
				alarms = append(alarms, alert("MIRROR_DAMAGED", "Повреждённая метка отражения: запись не изменена и не отражается обратно."))
			case driver.PartnerBooking:
				if !covered(it, inv[other].Items) {
					key := evidence(side, it)
					halves[key] = true
					if d.halves[key] {
						alarms = append(alarms, alert("MIRROR_HALF_BOOKING", "Бронь PadelApp второй проход без пары в другой CRM. Клубу и оператору: проверьте вручную; автоматическое исправление запрещено."))
					}
				}
			case driver.ClubBooking:
				found := false
				for _, m := range inv[other].Items {
					if m.Class == driver.MirrorBooking && valid(m, it, types[side]) {
						found = true
						break
					}
				}
				if !found {
					creates = append(creates, action{other, it})
				}
			case driver.MirrorBooking:
				if it.Source != "" && it.Source != types[other] {
					alarms = append(alarms, alert("MIRROR_DAMAGED", "Метка отражения указывает не на вторую CRM: запись не изменена."))
					continue
				}
				found := false
				for _, src := range inv[other].Items {
					if valid(it, src, types[other]) {
						found = true
						break
					}
				}
				if found || it.Start.Before(q.From) || it.End.After(q.To) {
					continue
				}
				key := evidence(side, it)
				absent[key] = true
				safe, err := d.sourceGone(ctx, ms[other], it, types[other], d.absent[key])
				if err != nil {
					alarms = append(alarms, alert("MIRROR_SOURCE_UNKNOWN", "Источник отражения не удалось проверить: отражение оставлено."))
					continue
				}
				if safe {
					deletes = append(deletes, action{side, it})
				}
			}
		}
	}
	d.absent, d.halves = absent, halves
	count := 0
	for _, a := range deletes {
		n := 1
		if c, ok := ms[a.side].(driver.RemovalCounter); ok {
			n = max(1, c.RemovalCount(a.item))
		}
		count += n
	}
	remove := count <= d.removals
	if !remove {
		alarms = append(alarms, alert("MIRROR_REMOVAL_FUSE", fmt.Sprintf("Запланировано снятий: %d; mirror.max_removals=%d. Снятия запрещены. Оператору: проверьте план, временно поднимите порог на один проход и верните его.", count, d.removals)))
	}
	if len(creates) >= d.creates {
		d.capped++
	} else {
		d.capped = 0
	}
	if d.capped >= 3 {
		alarms = append(alarms, alert("MIRROR_CREATE_LIMIT", fmt.Sprintf("Три прохода достигнут предел mirror.max_creates=%d; проверьте накопившиеся отражения.", d.creates)))
	}
	failed := false
	for _, a := range creates[:min(len(creates), d.creates)] {
		err := func() error {
			unlock, err := d.lock(ctx, a.item.Court)
			if err != nil {
				return err
			}
			defer unlock()
			// A booking moved or cancelled since the plan is not reflected from
			// that stale plan. Synthetic grid ids have no point-read endpoint.
			fresh, err := ms[1-a.side].Inspect(ctx, a.item.ID)
			if err != nil && !errors.Is(err, driver.ErrNotSupported) {
				return err
			}
			if err == nil {
				exists := false
				for _, it := range fresh {
					if it.Class == driver.ClubBooking && it.ID == a.item.ID && driver.SameInterval(it.Interval, a.item.Interval) {
						exists = true
					}
				}
				if !exists {
					return nil
				}
			}
			occ, err := ds[a.side].Occupancy(ctx, driver.OccupancyQuery{Courts: []string{a.item.Court}, From: a.item.Start, To: a.item.End})
			if err != nil {
				return err
			}
			if !occ.Complete {
				return driver.Upstream(driver.UpstreamInvalid, "mirror: incomplete target read")
			}
			if len(occ.Intervals) > 0 {
				return driver.ErrSlotUnavailable
			}
			return ms[a.side].CreateBlock(ctx, driver.BlockRequest{Interval: a.item.Interval, Source: types[1-a.side], SourceID: a.item.ID})
		}()
		if err != nil {
			failed = true
			alarms = append(alarms, alert("MIRROR_WRITE_FAILED", "Отражение не создано: цель занята или CRM отказала. Проверьте обе CRM; старые отражения оставлены."))
		}
	}
	// A failed move keeps the old block, so a refused replacement cannot
	// silently free the old slot. All creations precede all removals.
	if remove && !failed {
		for _, a := range deletes {
			err := func() error {
				unlock, err := d.lock(ctx, a.item.Court)
				if err != nil {
					return err
				}
				defer unlock()
				safe, err := d.sourceGone(ctx, ms[1-a.side], a.item, types[1-a.side], true)
				if err != nil || !safe {
					return err
				}
				return ms[a.side].DeleteBlock(ctx, a.item)
			}()
			if err != nil {
				alarms = append(alarms, alert("MIRROR_WRITE_FAILED", "Отражение не снято: источник или владение изменились либо CRM отказала. Проверьте обе CRM."))
			}
		}
	}
	return errors.Join(alarms...)
}
func (d *Driver) sourceGone(ctx context.Context, m driver.Mirrorer, it driver.InventoryItem, typ string, twice bool) (bool, error) {
	if it.SourceID == "" {
		return twice, nil
	}
	fresh, err := m.Inspect(ctx, it.SourceID)
	if errors.Is(err, driver.ErrNotSupported) {
		return twice, nil
	}
	if errors.Is(err, driver.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, s := range fresh {
		if valid(it, s, typ) {
			return false, nil
		}
	}
	return true, nil
}

// covered allows a CRM to tile one PadelApp booking into several records.
func covered(it driver.InventoryItem, items []driver.InventoryItem) bool {
	cursor := it.Start
	for cursor.Before(it.End) {
		next := cursor
		for _, x := range items {
			if x.Class == driver.PartnerBooking && x.Partner == it.Partner && x.Court == it.Court && !x.Start.After(cursor) && x.End.After(next) {
				next = x.End
			}
		}
		if next.Equal(cursor) {
			return false
		}
		cursor = next
	}
	return true
}
