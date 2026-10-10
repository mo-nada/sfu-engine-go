package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/mo-nada/sfu-engine/internal/cluster"
)

// Cluster is what the API needs to work on several nodes.
type Cluster interface {
	ID() string
	Owner(ctx context.Context, roomID string) (id, url string, err error)
	ClaimSelf(ctx context.Context, roomID string) (id, url string, err error)
	PickNode() (cluster.NodeInfo, error)
	Nodes() []cluster.NodeInfo
	Release(roomID string)
}

// Quota counts a tenant's rooms and participants across all nodes.
type Quota interface {
	TenantRooms(tenantID string) int
	TenantParticipants(tenantID string) int
}

func routed(r *http.Request) bool { return r.Header.Get(cluster.RoutedHeader) != "" }

// owned runs h on the node that owns the {room}: here, or forwarded there.
func (s *Server) owned(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cluster == nil || routed(r) {
			h(w, r)
			return
		}
		key, ok := roomKey(w, r)
		if !ok {
			return
		}
		id, owner, err := s.cluster.Owner(r.Context(), key)
		if err != nil {
			s.unavailable(w, err)
			return
		}
		if id == "" || id == s.cluster.ID() {
			h(w, r)
			return
		}
		s.proxy(w, r, owner, nil)
	}
}

// proxy forwards r to the node at base, marked as routed. If body is not
// nil, it replaces the request body (which was already read).
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, base string, body []byte) {
	target, err := url.Parse(base)
	if err != nil {
		s.unavailable(w, err)
		return
	}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Set(cluster.RoutedHeader, "1")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) { s.unavailable(w, err) },
	}
	rp.ServeHTTP(w, r)
}

// remoteRooms asks every other node for its rooms of the request's tenant.
func (s *Server) remoteRooms(r *http.Request) []roomJSON {
	var (
		mu  sync.Mutex
		out []roomJSON
		wg  sync.WaitGroup
	)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, n := range s.cluster.Nodes() {
		if n.ID == s.cluster.ID() {
			continue
		}
		wg.Go(func() {
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, n.URL+"/v1/rooms", nil)
			if err != nil {
				return
			}
			req.Header.Set("Authorization", r.Header.Get("Authorization"))
			req.Header.Set(cluster.RoutedHeader, "1")
			resp, err := client.Do(req)
			if err != nil {
				s.logger.Warn("room list from node failed", "node_id", n.ID, "err", err)
				return
			}
			defer resp.Body.Close()
			var body struct{ Rooms []roomJSON }
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil {
				mu.Lock()
				out = append(out, body.Rooms...)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

func (s *Server) unavailable(w http.ResponseWriter, err error) {
	s.logger.Warn("cluster unavailable", "err", err)
	writeError(w, http.StatusServiceUnavailable, "unavailable", "service temporarily unavailable, retry")
}
