package stargate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// SessionResolver validates a token hash and returns user + session or error.
type SessionResolver interface {
	Resolve(ctx context.Context, tokenHash string) (*auth.User, *auth.Session, error)
}

// Server runs the WebSocket server at /events. Use a separate process/domain
// (e.g. stargate.strafe.chat/events).
type Server struct {
	hub     *Hub
	resolve SessionResolver
	upgrade *websocket.Upgrader
}

// ServerConfig configures the WebSocket server.
type ServerConfig struct {
	Hub             *Hub
	Resolver        SessionResolver
	AllowedOrigins  []string // nil/empty = allow all
	ReadBufferSize  int
	WriteBufferSize int
}

func NewServer(cfg ServerConfig) *Server {
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  cfg.ReadBufferSize,
		WriteBufferSize: cfg.WriteBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			if len(cfg.AllowedOrigins) == 0 {
				return true // allow all when not configured (dev)
			}
			origin := r.Header.Get("Origin")
			for _, o := range cfg.AllowedOrigins {
				if o == origin {
					return true
				}
			}
			return false
		},
	}
	if upgrader.ReadBufferSize == 0 {
		upgrader.ReadBufferSize = 4096
	}
	if upgrader.WriteBufferSize == 0 {
		upgrader.WriteBufferSize = 4096
	}

	return &Server{
		hub:     cfg.Hub,
		resolve: cfg.Resolver,
		upgrade: upgrader,
	}
}

// ServeHTTP handles GET /events and upgrades to WebSocket.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/events" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := extractToken(r)
	if token == "" {
		s.writeError(w, r, 401, "missing or invalid authorization")
		return
	}

	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) == 0 {
		s.writeError(w, r, 401, "invalid token format")
		return
	}

	hash := sha256.Sum256(raw)
	tokenHash := hex.EncodeToString(hash[:])

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	user, session, err := s.resolve.Resolve(ctx, tokenHash)
	if err != nil {
		logger.Err("stargate", err, map[string]any{"path": "/events"})
		s.writeError(w, r, 500, "internal error")
		return
	}
	if user == nil || session == nil {
		s.writeError(w, r, 401, "invalid or expired session")
		return
	}

	conn, err := s.upgrade.Upgrade(w, r, nil)
	if err != nil {
		logger.Warn("stargate", "upgrade failed: %v", err)
		return
	}

	client := newClient(s.hub, conn, user.ID, session.SessionID)
	s.hub.register(client)

	// Auto-subscribe to own user stream for relationship requests, PMs, etc.
	client.subscribe("user", strconv.FormatInt(user.ID, 10))
	s.hub.subscribe(client, "user", strconv.FormatInt(user.ID, 10))

	client.sendOp(OpReady, ReadyPayload{
		User: map[string]interface{}{
			"id":            id.Format(user.ID),
			"username":      user.Username,
			"discriminator": user.Discriminator,
			"display_name":  user.DisplayName,
		},
		SessionID: strconv.FormatInt(session.SessionID, 10),
	})

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	go client.writePump(ctx2)
	client.readPump(ctx2)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]interface{}{
		"op": OpError,
		"d":  ErrorPayload{Code: status, Message: msg},
	})
	_, _ = w.Write(body)
}

func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if prefix := "Bearer "; strings.HasPrefix(h, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(h, prefix))
		}
	}
	if c, _ := r.Cookie("session_token"); c != nil && c.Value != "" {
		return c.Value
	}
	return r.URL.Query().Get("token")
}
