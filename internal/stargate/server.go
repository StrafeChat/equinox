package stargate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// SessionResolver validates a token hash and returns user + session or error. When it
// also implements BotResolver, `Authorization: Bot <token>` connections are accepted.
type SessionResolver interface {
	Resolve(ctx context.Context, tokenHash string) (*auth.User, *auth.Session, error)
}

// BotResolver validates a hashed bot token and returns the bot account (nil = unknown).
type BotResolver interface {
	ResolveBot(ctx context.Context, tokenHash string) (*auth.User, error)
}

// Server runs the WebSocket server at /events. Use a separate process/domain
// (e.g. stargate.strafe.chat/events).
type Server struct {
	hub       *Hub
	resolve   SessionResolver
	readyData ReadyDataProvider
	upgrade   *websocket.Upgrader
}

// ReadyDataProvider fetches initial data (rooms, relationships, spaces, space_rooms) for the READY payload.
// If nil, only user and session_id are sent.
type ReadyDataProvider interface {
	GetReadyData(ctx context.Context, userID int64) (*ReadyData, error)
}

// ServerConfig configures the WebSocket server.
type ServerConfig struct {
	Hub               *Hub
	Resolver          SessionResolver
	ReadyDataProvider ReadyDataProvider
	AllowedOrigins    []string // nil/empty = allow all
	ReadBufferSize    int
	WriteBufferSize   int
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
			// The origin check exists to stop a web page on another site from opening a
			// gateway connection with a signed-in user's ambient credentials. A client
			// that sends no Origin is not a browser (a bot, a native app): it holds its
			// token explicitly, so there is nothing for the check to protect - browsers
			// always send Origin on a WebSocket handshake.
			if origin == "" {
				return true
			}
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
		hub:       cfg.Hub,
		resolve:   cfg.Resolver,
		readyData: cfg.ReadyDataProvider,
		upgrade:   upgrader,
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

	scheme, token := extractToken(r)
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

	var user *auth.User
	var sessionID int64
	if scheme == "Bot" {
		// A bot has no session row: its identity is the application's token, hashed
		// the same way. sessionID stays 0 - nothing keys on it for bots.
		br, ok := s.resolve.(BotResolver)
		if !ok {
			s.writeError(w, r, 401, "bot tokens are not enabled")
			return
		}
		user, err = br.ResolveBot(ctx, tokenHash)
		if err != nil {
			logger.Err("stargate", err, map[string]any{"path": "/events", "scheme": "bot"})
			s.writeError(w, r, 500, "internal error")
			return
		}
		if user == nil {
			s.writeError(w, r, 401, "invalid bot token")
			return
		}
	} else {
		var session *auth.Session
		user, session, err = s.resolve.Resolve(ctx, tokenHash)
		if err != nil {
			logger.Err("stargate", err, map[string]any{"path": "/events"})
			s.writeError(w, r, 500, "internal error")
			return
		}
		if user == nil || session == nil {
			s.writeError(w, r, 401, "invalid or expired session")
			return
		}
		sessionID = session.SessionID
	}

	conn, err := s.upgrade.Upgrade(w, r, nil)
	if err != nil {
		logger.Warn("stargate", "upgrade failed: %v", err)
		return
	}

	client := newClient(s.hub, conn, user.ID, sessionID)
	s.hub.register(client)

	// Auto-subscribe to own user stream for relationship requests, PMs, etc.
	client.subscribe("user", strconv.FormatInt(user.ID, 10))
	s.hub.subscribe(client, "user", strconv.FormatInt(user.ID, 10))

	readyUser := map[string]interface{}{
		"id":            id.Format(user.ID),
		"username":      user.Username,
		"discriminator": fmt.Sprintf("%04d", user.Discriminator),
		"display_name":  user.DisplayName,
		"public_flags":  auth.PublicFlags(user),
		"bot":           user.Bot,
	}
	if user.Avatar != "" {
		readyUser["avatar"] = user.Avatar
	}
	if user.Banner != "" {
		readyUser["banner"] = user.Banner
	}
	if user.AboutMe != "" {
		readyUser["about_me"] = user.AboutMe
	}
	if user.Bio != "" {
		readyUser["bio"] = user.Bio
	}
	readyUser["pronouns"] = user.Pronouns
	readyUser["birthday_opt_in"] = user.BirthdayOptIn
	if b := auth.BirthdayMMDD(user); b != "" {
		readyUser["birthday"] = b
	}
	if p := auth.ToPublicPresence(user.Presence, false); p.Status != "" || p.CustomStatus != "" {
		readyUser["presence"] = p
	}
	readyPayload := ReadyPayload{
		User:      readyUser,
		SessionID: strconv.FormatInt(sessionID, 10),
	}
	if s.readyData != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		data, err := s.readyData.GetReadyData(ctx, user.ID)
		cancel()
		if err == nil && data != nil {
			readyPayload.Rooms = data.Rooms
			readyPayload.Relationships = data.Relationships
			readyPayload.Spaces = data.Spaces
			readyPayload.SpaceRooms = data.SpaceRooms
			readyPayload.VoiceStates = data.VoiceStates
			readyPayload.Calls = data.Calls
			// A bot is subscribed to everything it can see from the start, so a bot
			// program is "connect, read READY, handle events" with no subscribe
			// bookkeeping. Browser clients keep managing their own subscriptions (they
			// add and drop spaces live and already do this themselves).
			if user.Bot {
				for _, cid := range data.ChannelIDs {
					if client.subscribe("space", cid) {
						s.hub.subscribe(client, "space", cid)
					}
				}
			}
		}
	}
	client.sendOp(OpReady, readyPayload)

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

// extractToken returns the credential and its scheme: "Bearer" for a session token (from
// the Authorization header, the session_token cookie, or ?token= for browsers, which
// cannot set headers on a WebSocket) or "Bot" for a bot token, which is accepted from the
// Authorization header only - a bot can set headers, and a token in a URL leaks.
func extractToken(r *http.Request) (scheme, token string) {
	if h := r.Header.Get("Authorization"); h != "" {
		if prefix := "Bearer "; strings.HasPrefix(h, prefix) {
			return "Bearer", strings.TrimSpace(strings.TrimPrefix(h, prefix))
		}
		if prefix := "Bot "; strings.HasPrefix(h, prefix) {
			return "Bot", strings.TrimSpace(strings.TrimPrefix(h, prefix))
		}
	}
	if c, _ := r.Cookie("session_token"); c != nil && c.Value != "" {
		return "Bearer", c.Value
	}
	return "Bearer", r.URL.Query().Get("token")
}
