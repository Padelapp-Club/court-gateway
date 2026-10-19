package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/pair"
)

// MirrorInterval is how often the reconciliation loop of a club served by
// two CRMs runs: within it, a booking made directly in one CRM is blocked
// in the other and leaves the partners' sale (scenario 7: at most 2 min).
const MirrorInterval = time.Minute

// mirrorPassTimeout bounds one club's pass: the next is due a minute on.
const mirrorPassTimeout = 50 * time.Second

// reconciler is a club driver with a reconciliation loop (*pair.Pair).
type reconciler interface {
	Reconcile(ctx context.Context) pair.Report
}

// RunMirrors runs a pass of the loop of every served club that has one,
// at once and then every interval, until ctx is done. It is the gateway's
// only scheduled write: a club whose config the CRM refused (blocked) is
// left out; its alarms reach the hub with the club's readiness (the pair's
// Ready reports them as a config alert).
func RunMirrors(ctx context.Context, clubs *Clubs, interval time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for _, id := range clubs.IDs() {
			srv, ok := clubs.Server(id)
			if ctx.Err() != nil {
				return
			}
			r, paired := srv.Driver().(reconciler)
			if !ok || !paired || srv.mismatch.Load() {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, mirrorPassTimeout)
			rep := r.Reconcile(pctx)
			cancel()
			if len(rep.Created) > 0 || len(rep.Removed) > 0 || len(rep.Alarms) > 0 {
				log.Info("reconciliation pass", "club", id, "created", len(rep.Created), "removed", len(rep.Removed),
					"alarms", len(rep.Alarms), "aborted", rep.Aborted)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
