package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// ServerOps is the part of app.App the API uses for managed servers.
type ServerOps interface {
	ServerNames() []string
	Server(name string) (*server.Manager, error)
	PeerOp(ctx context.Context, trigger, srv, op, peer string, deploy bool) (*server.Peer, *server.DeployPlan, error)
	DeployServer(ctx context.Context, trigger, srv string, dryRun bool) (*server.DeployPlan, error)
}

// ServerInfo is one server in GET /servers.
type ServerInfo struct {
	Name        string    `json:"name"`
	Host        string    `json:"host,omitempty"`
	Local       bool      `json:"local,omitempty"`
	Endpoint    string    `json:"endpoint"`
	Interface   string    `json:"interface"`
	Subnet      string    `json:"subnet"`
	Initialized bool      `json:"initialized"`
	PublicKey   string    `json:"public_key,omitempty"`
	Peers       int       `json:"peers"`
	DeployedAt  time.Time `json:"deployed_at,omitzero"`
	Error       string    `json:"error,omitempty"`
}

// PeerInfo is a client without its keys.
type PeerInfo struct {
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	PublicKey string    `json:"public_key"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	RotatedAt time.Time `json:"rotated_at,omitzero"`
}

func peerInfo(p *server.Peer) PeerInfo {
	return PeerInfo{Name: p.Name, Address: p.Address, PublicKey: p.Keys.Public, Disabled: p.Disabled, CreatedAt: p.CreatedAt, RotatedAt: p.RotatedAt}
}

// ServerOpResult is the answer of peer operations and deploys.
type ServerOpResult struct {
	OK    bool               `json:"ok"`
	Peer  *PeerInfo          `json:"peer,omitempty"`
	Plan  *server.DeployPlan `json:"deploy,omitempty"`
	Error string             `json:"error,omitempty"`
}

func (s *Server) serverRoutes(read, write, secret func(string, http.HandlerFunc)) {
	read("GET /api/v1/servers", s.serverList)
	read("GET /api/v1/servers/{server}/status", s.serverStatus)
	read("GET /api/v1/servers/{server}/peers", s.peerList)
	write("POST /api/v1/servers/{server}/peers", s.peerAdd)
	write("DELETE /api/v1/servers/{server}/peers/{peer}", s.peerOp(server.PeerRemove))
	write("POST /api/v1/servers/{server}/peers/{peer}/rotate", s.peerOp(server.PeerRotate))
	write("POST /api/v1/servers/{server}/peers/{peer}/disable", s.peerOp(server.PeerDisable))
	write("POST /api/v1/servers/{server}/peers/{peer}/enable", s.peerOp(server.PeerEnable))
	write("POST /api/v1/servers/{server}/deploy", s.serverDeploy)
	// Client configurations carry private keys: full token only.
	secret("GET /api/v1/servers/{server}/peers/{peer}/conf", s.peerConf)
	secret("GET /api/v1/servers/{server}/peers/{peer}/qr.png", s.peerQR)
	secret("GET /api/v1/servers/{server}/peers/{peer}/vpn", s.peerVPN)
}

func (s *Server) manager(w http.ResponseWriter, r *http.Request) (*server.Manager, bool) {
	if s.Servers == nil {
		writeError(w, http.StatusNotFound, errors.New("no servers configured"))
		return nil, false
	}
	m, err := s.Servers.Server(r.PathValue("server"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return nil, false
	}
	return m, true
}

func (s *Server) serverList(w http.ResponseWriter, _ *http.Request) {
	out := []ServerInfo{}
	if s.Servers == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	for _, n := range s.Servers.ServerNames() {
		si := ServerInfo{Name: n}
		m, err := s.Servers.Server(n)
		if err != nil {
			si.Error = err.Error()
			out = append(out, si)
			continue
		}
		c := m.Config
		si.Host, si.Local, si.Endpoint, si.Interface, si.Subnet = c.Host, c.Local, c.ClientEndpoint(), c.Interface, c.Subnet
		switch st, err := m.Load(); {
		case err == nil:
			si.Initialized, si.PublicKey, si.Peers, si.DeployedAt = true, st.Keys.Public, len(st.Peers), st.DeployedAt
		case !errors.Is(err, server.ErrNotInitialized):
			si.Error = err.Error()
		}
		out = append(out, si)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) serverStatus(w http.ResponseWriter, r *http.Request) {
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	st, err := m.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) peerList(w http.ResponseWriter, r *http.Request) {
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	st, err := m.Load()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	out := make([]PeerInfo, 0, len(st.Peers))
	for i := range st.Peers {
		out = append(out, peerInfo(&st.Peers[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

type peerRequest struct {
	Name   string `json:"name"`
	Deploy *bool  `json:"deploy"` // default true
}

func (s *Server) peerAdd(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.manager(w, r); !ok {
		return
	}
	var req peerRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.runPeerOp(w, r, server.PeerAdd, req.Name, req.Deploy == nil || *req.Deploy)
}

func (s *Server) peerOp(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.manager(w, r); !ok {
			return
		}
		var req struct {
			Deploy *bool `json:"deploy"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		deploy := req.Deploy == nil || *req.Deploy
		if r.URL.Query().Get("deploy") == "false" {
			deploy = false
		}
		s.runPeerOp(w, r, op, r.PathValue("peer"), deploy)
	}
}

func (s *Server) runPeerOp(w http.ResponseWriter, r *http.Request, op, peer string, deploy bool) {
	p, plan, err := s.Servers.PeerOp(r.Context(), syncer.TriggerAPI, r.PathValue("server"), op, peer, deploy)
	out := ServerOpResult{OK: err == nil, Plan: plan}
	if p != nil {
		pi := peerInfo(p)
		out.Peer = &pi
	}
	code := http.StatusOK
	if op == server.PeerAdd && p != nil {
		code = http.StatusCreated
	}
	if err != nil {
		out.Error = err.Error()
		code = http.StatusUnprocessableEntity
	}
	writeJSON(w, code, out)
}

func (s *Server) serverDeploy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.manager(w, r); !ok {
		return
	}
	var req struct {
		DryRun bool `json:"dry_run"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	plan, err := s.Servers.DeployServer(r.Context(), syncer.TriggerAPI, r.PathValue("server"), req.DryRun)
	out := ServerOpResult{OK: err == nil, Plan: plan}
	code := http.StatusOK
	if err != nil {
		out.Error = err.Error()
		code = http.StatusUnprocessableEntity
	}
	writeJSON(w, code, out)
}

func (s *Server) clientConf(w http.ResponseWriter, r *http.Request) (string, *server.Manager, bool) {
	m, ok := s.manager(w, r)
	if !ok {
		return "", nil, false
	}
	conf, err := m.ClientConf(r.PathValue("peer"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return "", nil, false
	}
	s.logger().Info("api: client configuration exported", "server", m.Config.Name, "peer", r.PathValue("peer"), "path", r.URL.Path, "remote", r.RemoteAddr)
	return conf, m, true
}

func (s *Server) peerConf(w http.ResponseWriter, r *http.Request) {
	conf, m, ok := s.clientConf(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+m.Config.Name+"-"+r.PathValue("peer")+`.conf"`)
	_, _ = w.Write([]byte(conf))
}

func (s *Server) peerQR(w http.ResponseWriter, r *http.Request) {
	conf, _, ok := s.clientConf(w, r)
	if !ok {
		return
	}
	png, err := server.QRPNG(conf, 512)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (s *Server) peerVPN(w http.ResponseWriter, r *http.Request) {
	m, ok := s.manager(w, r)
	if !ok {
		return
	}
	key, err := m.VPNKey(r.PathValue("peer"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.logger().Info("api: client configuration exported", "server", m.Config.Name, "peer", r.PathValue("peer"), "path", r.URL.Path, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(key))
}
