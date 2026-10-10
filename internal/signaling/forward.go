package signaling

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mo-nada/sfu-engine/internal/cluster"
)

// RoutedHeader marks a request forwarded by another node (see cluster.RoutedHeader).
const RoutedHeader = cluster.RoutedHeader

// Routing decides which node handles a join.
type Routing interface {
	// RouteJoin returns "" if this node handles the join (claiming the room
	// for this node if needed), or the base URL of the node to forward it to.
	// routed is true for a join another node forwarded here.
	RouteJoin(ctx context.Context, roomID string, autoCreate, routed bool) (forwardURL string, err error)
	// Release gives up a room claim made by RouteJoin when the join failed.
	Release(roomID string)
}

// Quota counts a tenant's rooms and participants across all nodes.
type Quota interface {
	TenantRooms(tenantID string) int
	TenantParticipants(tenantID string) int
}

func (s *session) upstreamConn() *websocket.Conn {
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	return s.upstream
}

// forward connects to the room's node and relays the session there: the
// join is sent again, and from then on every message passes through
// unchanged in both directions. Media does not pass through this node: ICE
// candidates come from the room's node, so clients connect to it directly.
func (s *session) forward(baseURL, tok string, logger *slog.Logger) {
	wsURL := "ws" + strings.TrimPrefix(strings.TrimSuffix(baseURL, "/"), "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	up, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, http.Header{RoutedHeader: {"1"}})
	if err != nil {
		logger.Warn("forward to room node failed", "url", baseURL, "err", err)
		s.sendError("room unavailable")
		return
	}
	up.SetReadLimit(maxMessageSize)
	join, _ := encodeJoin(tok)
	if err := up.WriteMessage(websocket.TextMessage, join); err != nil {
		_ = up.Close()
		s.sendError("room unavailable")
		return
	}
	s.upstreamMu.Lock()
	s.upstream = up
	s.upstreamMu.Unlock()
	if s.closed.Load() { // closed meanwhile: close has not seen upstream
		_ = up.Close()
		return
	}
	logger.Info("join forwarded to room node", "url", baseURL)

	go func() {
		for {
			_, data, err := up.ReadMessage()
			if err != nil {
				go s.close()
				return
			}
			s.sendRaw(data)
		}
	}()
}

func encodeJoin(tok string) ([]byte, error) {
	m := Message{Type: TypeJoin, Token: tok}
	return marshal(m)
}
