package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/domains"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// GroupInfo is one group in GET /groups.
type GroupInfo struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Connection    string   `json:"connection"`
	Sources       []string `json:"sources,omitempty"`
	Latest        int      `json:"latest_version"`
	LatestAt      string   `json:"latest_at,omitempty"`
	Entries       int      `json:"entries"`
	Exclude       int      `json:"exclude"`
	Applied       int      `json:"applied_version"`
	Pinned        int      `json:"pinned_version,omitempty"`
	ManualInclude []string `json:"manual_include,omitempty"`
	ManualRemove  []string `json:"manual_remove,omitempty"`
	Error         string   `json:"error,omitempty"`
}

func (s *Server) spec() (*domains.Spec, error) { return domains.Load(s.SpecPath) }

func (s *Server) groupInfo(g *domains.Group) GroupInfo {
	gi := GroupInfo{Name: g.Name, Description: g.Description, Connection: g.Connection}
	for _, src := range g.Sources {
		gi.Sources = append(gi.Sources, src.Label())
	}
	st, err := s.Store.GroupState(g.Name)
	if err != nil {
		gi.Error = err.Error()
		return gi
	}
	gi.Applied, gi.Pinned, gi.ManualInclude, gi.ManualRemove = st.Applied, st.Pinned, st.ManualInclude, st.ManualRemove
	if v, err := s.Store.Latest(g.Name); err != nil {
		gi.Error = err.Error()
	} else if v != nil {
		gi.Latest, gi.Entries, gi.Exclude = v.Number, len(v.Entries), len(v.Exclude)
		gi.LatestAt = v.CreatedAt.UTC().Format(time.RFC3339)
	}
	return gi
}

func (s *Server) groups(w http.ResponseWriter, _ *http.Request) {
	sp, err := s.spec()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]GroupInfo, 0, len(sp.Groups))
	for i := range sp.Groups {
		out = append(out, s.groupInfo(&sp.Groups[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// specGroup finds the group or writes 404.
func (s *Server) specGroup(w http.ResponseWriter, r *http.Request) (*domains.Group, bool) {
	sp, err := s.spec()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return nil, false
	}
	g, ok := sp.Group(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown group %q", r.PathValue("name")))
		return nil, false
	}
	return g, true
}

// GroupDetail is GET /groups/{name}: the group and one version's entries.
type GroupDetail struct {
	GroupInfo
	Version *store.Version `json:"version,omitempty"`
}

func (s *Server) group(w http.ResponseWriter, r *http.Request) {
	g, ok := s.specGroup(w, r)
	if !ok {
		return
	}
	d := GroupDetail{GroupInfo: s.groupInfo(g)}
	var err error
	if q := r.URL.Query().Get("version"); q != "" {
		n, perr := strconv.Atoi(q)
		if perr != nil || n < 1 {
			writeError(w, http.StatusBadRequest, errors.New("version must be a positive number"))
			return
		}
		d.Version, err = s.Store.Version(g.Name, n)
	} else {
		d.Version, err = s.Store.Latest(g.Name)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, d)
	}
}

// VersionInfo is one entry of GET /groups/{name}/versions.
type VersionInfo struct {
	Number    int                `json:"number"`
	CreatedAt time.Time          `json:"created_at"`
	Entries   int                `json:"entries"`
	Exclude   int                `json:"exclude"`
	Reason    string             `json:"reason,omitempty"`
	Truncated int                `json:"truncated,omitempty"`
	Sources   []store.SourceStat `json:"sources,omitempty"`
	Applied   bool               `json:"applied"`
	Pinned    bool               `json:"pinned"`
}

func (s *Server) versions(w http.ResponseWriter, r *http.Request) {
	g, ok := s.specGroup(w, r)
	if !ok {
		return
	}
	st, err := s.Store.GroupState(g.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	vs, err := s.Store.Versions(g.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]VersionInfo, 0, len(vs))
	for i := len(vs) - 1; i >= 0; i-- {
		v := vs[i]
		out = append(out, VersionInfo{Number: v.Number, CreatedAt: v.CreatedAt, Entries: len(v.Entries), Exclude: len(v.Exclude),
			Reason: v.Reason, Truncated: v.Truncated, Sources: v.Sources, Applied: v.Number == st.Applied, Pinned: v.Number == st.Pinned})
	}
	writeJSON(w, http.StatusOK, out)
}

// OpResult is the answer of sync, rollback and tunnel apply: the journal
// view of the operation plus the human-readable summary and plan.
type OpResult struct {
	OK      bool           `json:"ok"`
	Entry   *journal.Entry `json:"entry"`
	Summary string         `json:"summary,omitempty"`
	Plan    string         `json:"plan,omitempty"`
	Error   string         `json:"error,omitempty"`
}

func (s *Server) syncResult(w http.ResponseWriter, kind string, dryRun bool, started time.Time, res *syncer.Result, err error) {
	e := journal.FromReport(&syncer.Report{Kind: kind, Trigger: syncer.TriggerAPI, DryRun: dryRun, Started: started,
		Duration: time.Since(started), Result: res, Err: err}, s.MaxChanges)
	out := OpResult{OK: err == nil, Entry: e}
	if res != nil {
		out.Summary = res.Summary()
		if res.Apply != nil && res.Apply.Plan != nil {
			out.Plan = res.Apply.Plan.String()
		}
	}
	code := http.StatusOK
	if err != nil {
		out.Error = err.Error()
		code = http.StatusUnprocessableEntity
		if errors.Is(err, router.ErrLocked) {
			code = http.StatusConflict
		}
	}
	writeJSON(w, code, out)
}

type syncRequest struct {
	Groups []string `json:"groups"`
	DryRun bool     `json:"dry_run"`
	Force  bool     `json:"force"`
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	var req syncRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	started := time.Now()
	res, err := s.Syncer.Run(r.Context(), syncer.Options{Groups: req.Groups, DryRun: req.DryRun, Force: req.Force, Trigger: syncer.TriggerAPI})
	s.syncResult(w, syncer.KindSync, req.DryRun, started, res, err)
}

type rollbackRequest struct {
	To     int  `json:"to"`
	DryRun bool `json:"dry_run"`
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	g, ok := s.specGroup(w, r)
	if !ok {
		return
	}
	var req rollbackRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	started := time.Now()
	res, err := s.Syncer.Rollback(r.Context(), g.Name, req.To, req.DryRun)
	if res == nil && err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.syncResult(w, syncer.KindRollback, req.DryRun, started, res, err)
}

type manualRequest struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
	Clear  bool     `json:"clear"`
	// Apply syncs the group right after the edit.
	Apply bool `json:"apply"`
}

func (s *Server) manual(w http.ResponseWriter, r *http.Request) {
	g, ok := s.specGroup(w, r)
	if !ok {
		return
	}
	var req manualRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !req.Clear && len(req.Add) == 0 && len(req.Remove) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("nothing to do: set add, remove or clear"))
		return
	}
	if req.Clear {
		if err := s.Syncer.ClearManual(g.Name); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if len(req.Add) > 0 {
		if _, err := s.Syncer.AddManual(g.Name, req.Add); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if len(req.Remove) > 0 {
		if _, err := s.Syncer.RemoveManual(g.Name, req.Remove); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	s.afterEdit(w, r, g, req.Apply)
}

type unpinRequest struct {
	Apply bool `json:"apply"`
}

func (s *Server) unpin(w http.ResponseWriter, r *http.Request) {
	g, ok := s.specGroup(w, r)
	if !ok {
		return
	}
	var req unpinRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Syncer.Unpin(g.Name); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.afterEdit(w, r, g, req.Apply)
}

// afterEdit answers with the group, or syncs it first when asked.
func (s *Server) afterEdit(w http.ResponseWriter, r *http.Request, g *domains.Group, apply bool) {
	if !apply {
		writeJSON(w, http.StatusOK, s.groupInfo(g))
		return
	}
	started := time.Now()
	res, err := s.Syncer.Run(r.Context(), syncer.Options{Groups: []string{g.Name}, Trigger: syncer.TriggerAPI})
	s.syncResult(w, syncer.KindSync, false, started, res, err)
}

func (s *Server) journalList(w http.ResponseWriter, r *http.Request) {
	if s.Journal == nil {
		writeError(w, http.StatusNotFound, errors.New("journal is disabled"))
		return
	}
	q := r.URL.Query()
	f := journal.Filter{Group: q.Get("group"), Kind: q.Get("kind"), Status: q.Get("status"), Limit: 50}
	if v := q.Get("since"); v != "" {
		t, err := journal.ParseSince(v, s.now())
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("since %w", err))
			return
		}
		f.Since = t
	}
	if v := q.Get("dry_run"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("dry_run must be true or false"))
			return
		}
		f.DryRun = &b
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 10000 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be 0..10000"))
			return
		}
		f.Limit = n
	}
	es, err := s.Journal.Query(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if es == nil {
		es = []*journal.Entry{}
	}
	writeJSON(w, http.StatusOK, es)
}

func (s *Server) journalEntry(w http.ResponseWriter, r *http.Request) {
	if s.Journal == nil {
		writeError(w, http.StatusNotFound, errors.New("journal is disabled"))
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("id must be a number"))
		return
	}
	e, err := s.Journal.Get(id)
	switch {
	case errors.Is(err, journal.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, e)
	}
}

func (s *Server) alerts(w http.ResponseWriter, _ *http.Request) {
	st, err := s.Alerter.States()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if st == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// CheckResult is one health check outcome.
type CheckResult struct {
	Key   string  `json:"key"`
	OK    bool    `json:"ok"`
	Error string  `json:"error,omitempty"`
	TookS float64 `json:"took_seconds"`
}

func (s *Server) checkResults() []CheckResult {
	out := []CheckResult{}
	if s.Checks == nil {
		return out
	}
	for _, r := range s.Checks() {
		c := CheckResult{Key: r.Signal.Key(), OK: r.Signal.Err == nil, TookS: r.Took.Seconds()}
		if r.Signal.Err != nil {
			c.Error = r.Signal.Err.Error()
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) checks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.checkResults())
}

// RouterInterface is a Wireguard interface without key material.
type RouterInterface struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Address       string   `json:"address,omitempty"`
	MTU           int      `json:"mtu,omitempty"`
	Up            bool     `json:"up"`
	ASC           []string `json:"asc,omitempty"`
	Peers         []string `json:"peers,omitempty"` // public keys
	SecurityLevel string   `json:"security_level,omitempty"`
}

// RouterState is GET /router.
type RouterState struct {
	Groups []struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Include     int    `json:"include"`
		Exclude     int    `json:"exclude"`
	} `json:"groups"`
	Routes     []router.DNSRoute `json:"routes"`
	Interfaces []RouterInterface `json:"interfaces"`
}

func (s *Server) routerState(w http.ResponseWriter, r *http.Request) {
	st, err := s.Router.ReadState(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	var out RouterState
	for _, g := range st.Groups {
		out.Groups = append(out.Groups, struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			Include     int    `json:"include"`
			Exclude     int    `json:"exclude"`
		}{g.Name, g.Description, len(g.Include), len(g.Exclude)})
	}
	out.Routes = st.Routes
	for _, i := range st.Interfaces {
		ri := RouterInterface{Name: i.Name, Description: i.Description, Address: i.Address, MTU: i.MTU, Up: i.Up, ASC: i.ASC, SecurityLevel: i.SecurityLevel}
		for _, p := range i.Peers {
			ri.Peers = append(ri.Peers, p.PublicKey)
		}
		out.Interfaces = append(out.Interfaces, ri)
	}
	writeJSON(w, http.StatusOK, out)
}

// TunnelInfo is one tunnel in GET /tunnels.
type TunnelInfo struct {
	Name      string    `json:"name"`
	Interface string    `json:"interface,omitempty"`
	Auto      bool      `json:"auto_interface"`
	Enabled   bool      `json:"enabled"`
	AppliedAt time.Time `json:"applied_at,omitzero"`
	Orphan    bool      `json:"orphan,omitempty"`   // recorded but no longer configured
	Imported  bool      `json:"imported,omitempty"` // key added with awg import / the API
}

func (s *Server) tunnelsConfigured(w http.ResponseWriter) bool {
	if s.Tunnels == nil {
		writeError(w, http.StatusNotFound, errors.New("no awg tunnels configured"))
		return false
	}
	return true
}

func (s *Server) tunnels(w http.ResponseWriter, _ *http.Request) {
	out := []TunnelInfo{}
	if s.Tunnels == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	recs, err := s.Tunnels.Records()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	seen := map[string]bool{}
	for _, t := range s.Tunnels.List() {
		seen[t.Name] = true
		ti := TunnelInfo{Name: t.Name, Interface: t.Interface, Auto: t.AutoInterface(), Enabled: t.IsUp(), AppliedAt: recs[t.Name].AppliedAt, Imported: t.Imported}
		if ti.Auto {
			ti.Interface = recs[t.Name].Interface
		}
		out = append(out, ti)
	}
	var orphans []string
	for n := range recs {
		if !seen[n] {
			orphans = append(orphans, n)
		}
	}
	sort.Strings(orphans)
	for _, n := range orphans {
		out = append(out, TunnelInfo{Name: n, Interface: recs[n].Interface, AppliedAt: recs[n].AppliedAt, Orphan: true})
	}
	writeJSON(w, http.StatusOK, out)
}

// TunnelStatus is one tunnel in GET /tunnels/status.
type TunnelStatus struct {
	Tunnel    string           `json:"tunnel"`
	Interface string           `json:"interface,omitempty"`
	Healthy   bool             `json:"healthy"`
	Error     string           `json:"error,omitempty"`
	Up        bool             `json:"up"`
	Peers     []TunnelPeerInfo `json:"peers,omitempty"`
}

// TunnelPeerInfo is the runtime state of one peer.
type TunnelPeerInfo struct {
	PublicKey     string  `json:"public_key"`
	Endpoint      string  `json:"endpoint,omitempty"`
	Online        bool    `json:"online"`
	HandshakeAgeS float64 `json:"handshake_age_seconds"` // -1 = never
	RxBytes       uint64  `json:"rx_bytes"`
	TxBytes       uint64  `json:"tx_bytes"`
}

func (s *Server) tunnelStatus(w http.ResponseWriter, r *http.Request) {
	if !s.tunnelsConfigured(w) {
		return
	}
	sts, err := s.Tunnels.Statuses(r.Context(), r.URL.Query()["tunnel"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out := make([]TunnelStatus, 0, len(sts))
	for _, st := range sts {
		ts := TunnelStatus{Tunnel: st.Tunnel, Interface: st.Interface, Healthy: st.Err == nil}
		if st.Err != nil {
			ts.Error = st.Err.Error()
		}
		if st.Router != nil {
			ts.Up = st.Router.Up
			for _, p := range st.Router.Peers {
				age := -1.0
				if p.LastHandshake >= 0 {
					age = p.LastHandshake.Seconds()
				}
				ts.Peers = append(ts.Peers, TunnelPeerInfo{PublicKey: p.PublicKey, Endpoint: p.Endpoint, Online: p.Online,
					HandshakeAgeS: age, RxBytes: p.RxBytes, TxBytes: p.TxBytes})
			}
		}
		out = append(out, ts)
	}
	writeJSON(w, http.StatusOK, out)
}

type tunnelApplyRequest struct {
	Tunnels   []string `json:"tunnels"`
	DryRun    bool     `json:"dry_run"`
	ForceKeys bool     `json:"force_keys"`
}

// TunnelApplyResult is the answer of POST /tunnels/apply.
type TunnelApplyResult struct {
	OpResult
	Items []awg.Item `json:"tunnels"`
}

func (s *Server) tunnelApply(w http.ResponseWriter, r *http.Request) {
	if !s.tunnelsConfigured(w) {
		return
	}
	var req tunnelApplyRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	started := time.Now()
	res, err := s.Tunnels.Apply(r.Context(), req.Tunnels, awg.ApplyOptions{DryRun: req.DryRun, ForceKeys: req.ForceKeys})
	var ar *router.ApplyResult
	out := TunnelApplyResult{}
	if res != nil {
		ar, out.Items = res.Apply, res.Items
	}
	e := journal.FromApply(started, time.Since(started), req.DryRun, ar, err, s.MaxChanges)
	e.Kind, e.Trigger = journal.KindAWGApply, syncer.TriggerAPI
	if s.Record != nil && res != nil {
		s.Record(e)
	}
	out.OK, out.Entry = err == nil, e
	if ar != nil && ar.Plan != nil {
		out.Plan = ar.Plan.String()
	}
	code := http.StatusOK
	if err != nil {
		out.Error = err.Error()
		code = http.StatusUnprocessableEntity
	}
	writeJSON(w, code, out)
}

type tunnelImportRequest struct {
	Name    string `json:"name"`
	Key     string `json:"key"`   // .conf text or vpn:// key
	Apply   *bool  `json:"apply"` // default true
	Replace bool   `json:"replace"`
}

// tunnelImport stores a client key of any AmneziaWG server and imports it.
func (s *Server) tunnelImport(w http.ResponseWriter, r *http.Request) {
	if s.ImportKey == nil {
		writeError(w, http.StatusNotFound, errors.New("key import is not available"))
		return
	}
	var req tunnelImportRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.ImportKey(r.Context(), syncer.TriggerAPI, req.Name, []byte(req.Key), req.Apply == nil || *req.Apply, req.Replace)
	s.tunnelOpResult(w, res, err)
}

// tunnelForget removes an imported tunnel from the router and its key.
func (s *Server) tunnelForget(w http.ResponseWriter, r *http.Request) {
	if s.ForgetKey == nil {
		writeError(w, http.StatusNotFound, errors.New("key import is not available"))
		return
	}
	res, err := s.ForgetKey(r.Context(), syncer.TriggerAPI, r.PathValue("name"))
	s.tunnelOpResult(w, res, err)
}

func (s *Server) tunnelOpResult(w http.ResponseWriter, res *awg.Result, err error) {
	out := TunnelApplyResult{OpResult: OpResult{OK: err == nil}}
	if res != nil {
		out.Items = res.Items
		if res.Apply != nil && res.Apply.Plan != nil {
			out.Plan = res.Apply.Plan.String()
		}
	}
	code := http.StatusOK
	if err != nil {
		out.Error = err.Error()
		code = http.StatusUnprocessableEntity
	}
	writeJSON(w, code, out)
}

// Status is GET /status: a one-call overview for the UI.
type Status struct {
	Version  string           `json:"version"`
	Started  time.Time        `json:"started"`
	Groups   int              `json:"groups"`
	Tunnels  int              `json:"tunnels"`
	Servers  int              `json:"servers"`
	LastSync *journal.Entry   `json:"last_sync,omitempty"`
	Alerts   []map[string]any `json:"active_alerts"`
	Checks   []CheckResult    `json:"checks"`
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st := Status{Version: s.Version, Started: s.started, Alerts: []map[string]any{}, Checks: s.checkResults()}
	if sp, err := s.spec(); err == nil {
		st.Groups = len(sp.Groups)
	}
	if s.Tunnels != nil {
		st.Tunnels = len(s.Tunnels.List())
	}
	if s.Servers != nil {
		st.Servers = len(s.Servers.ServerNames())
	}
	if s.Journal != nil {
		if es, err := s.Journal.Query(journal.Filter{Kind: syncer.KindSync, Limit: 1}); err == nil && len(es) > 0 {
			st.LastSync = es[0]
		}
	}
	if as, err := s.Alerter.States(); err == nil {
		for _, a := range as {
			if a.Active {
				st.Alerts = append(st.Alerts, map[string]any{"key": a.Alert + suffix(a.Check), "since": a.Since, "failures": a.Failures, "detail": a.Detail})
			}
		}
	}
	writeJSON(w, http.StatusOK, st)
}

func suffix(check string) string {
	if check == "" {
		return ""
	}
	return ":" + check
}
