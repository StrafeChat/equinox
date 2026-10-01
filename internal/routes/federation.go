package routes

import (
	"github.com/StrafeChat/equinox/internal/federation"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// SetupFederationRoutes mounts the instance document and the signed server-to-server API.
// No-op when federation is off (FEDERATION_DOMAIN unset).
func SetupFederationRoutes(d Deps) {
	if d.Federation == nil {
		return
	}
	h := federation.NewHandler(d.Federation)

	// Discovery: peers and clients read this to find the API/gateway and the signing key.
	d.App.Get(federation.WellKnownPath, h.Instance)

	g := d.App.Group(federation.BasePath)
	g.Get("/instance", h.Instance)

	s2s := d.Federation.RequireInstance()
	g.Get("/users/lookup", s2s, h.LookupUser)
	g.Get("/users/:id", s2s, h.GetUser)
	g.Post("/users/update", s2s, h.UserUpdated)
	g.Post("/users/presence", s2s, h.Presence)
	g.Post("/relationships", s2s, h.Relationship)
	g.Post("/rooms/reactions", s2s, h.ReactionAdd)
	g.Post("/rooms/reactions/delete", s2s, h.ReactionRemove)
	// Calls: the first five are asked of the room's origin, the last two are what the
	// origin pushes to everyone else.
	g.Post("/rooms/voice/join", s2s, h.VoiceJoin)
	g.Post("/rooms/voice/leave", s2s, h.VoiceLeave)
	g.Post("/rooms/voice/self", s2s, h.VoiceSelf)
	g.Post("/rooms/voice/ring", s2s, h.VoiceRing)
	g.Post("/rooms/voice/decline", s2s, h.VoiceDecline)
	g.Post("/rooms/voice/state", s2s, h.VoiceState)
	g.Post("/rooms/voice/call", s2s, h.VoiceCall)
	g.Post("/rooms", s2s, h.RoomCreate)
	g.Put("/rooms/participants", s2s, h.RoomParticipants)
	g.Patch("/rooms", s2s, h.RoomPatch)
	g.Post("/rooms/typing", s2s, h.RoomTyping)
	g.Post("/rooms/messages", s2s, h.MessageCreate)
	g.Patch("/rooms/messages", s2s, h.MessageEdit)
	g.Post("/rooms/messages/delete", s2s, h.MessageDelete)
	g.Post("/keys/query", s2s, h.KeysQuery)
	g.Post("/keys/claim", s2s, h.KeysClaim)
	g.Post("/to_device", s2s, h.ToDevice)

	// Client-facing (session auth): which instances this one has federated with.
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(d.Scylla), d.Redis, d.Config)
	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(d.Scylla), d.Redis, d.Config)
	requireAuth := middleware.RequireAuth(sessionRepo, userRepo)
	d.App.Get("/federation/peers", requireAuth, h.Peers)
}
