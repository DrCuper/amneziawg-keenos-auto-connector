package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// metrics serves a small Prometheus text exposition: check and alert state,
// group sizes and the last sync outcome. Everything is read on request.
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	gauge("awgkeen_up", "Service is running.")
	b.WriteString("awgkeen_up 1\n")
	gauge("awgkeen_start_time_seconds", "Unix time the service started.")
	fmt.Fprintf(&b, "awgkeen_start_time_seconds %d\n", s.started.Unix())

	gauge("awgkeen_check_ok", "Last result of a health check (1 = ok).")
	for _, c := range s.checkResults() {
		fmt.Fprintf(&b, "awgkeen_check_ok{check=%q} %d\n", c.Key, b2i(c.OK))
	}
	if as, err := s.Alerter.States(); err == nil {
		gauge("awgkeen_alert_active", "Alert is firing (1) or not (0).")
		sort.Slice(as, func(i, j int) bool { return as[i].Alert+as[i].Check < as[j].Alert+as[j].Check })
		for _, a := range as {
			fmt.Fprintf(&b, "awgkeen_alert_active{alert=%q} %d\n", a.Alert+suffix(a.Check), b2i(a.Active))
		}
	}
	if sp, err := s.spec(); err == nil {
		gauge("awgkeen_group_entries", "Entries in the latest version of a group.")
		gauge2 := strings.Builder{}
		for i := range sp.Groups {
			gi := s.groupInfo(&sp.Groups[i])
			fmt.Fprintf(&b, "awgkeen_group_entries{group=%q} %d\n", gi.Name, gi.Entries)
			fmt.Fprintf(&gauge2, "awgkeen_group_applied_version{group=%q} %d\n", gi.Name, gi.Applied)
		}
		gauge("awgkeen_group_applied_version", "Version of a group applied to the router (0 = never).")
		b.WriteString(gauge2.String())
	}
	if s.Journal != nil {
		if es, err := s.Journal.Query(journal.Filter{Kind: syncer.KindSync, Limit: 1}); err == nil && len(es) > 0 {
			e := es[0]
			gauge("awgkeen_last_sync_timestamp_seconds", "Unix time of the last sync (dry-run included).")
			fmt.Fprintf(&b, "awgkeen_last_sync_timestamp_seconds %d\n", e.Time.Unix())
			gauge("awgkeen_last_sync_ok", "Last sync finished without errors (1) or not (0).")
			fmt.Fprintf(&b, "awgkeen_last_sync_ok %d\n", b2i(e.Status == journal.StatusOK))
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}
