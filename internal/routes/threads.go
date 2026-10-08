package routes

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/modules/threads"
)

// registerThreadRoutes mounts the thread endpoints. The service is built by the messages
// route setup, which also hands it to the messages service as its thread hooks (a message
// in a thread touches the thread's activity, counts and membership).
func registerThreadRoutes(d Deps, requireAuth fiber.Handler, svc *threads.Service) {
	svc.StartArchiveWorker(context.Background())
	h := threads.NewHandler(svc)

	r := d.App.Group("/rooms", requireAuth)
	r.Post("/:id/messages/:msg_id/threads", perUserLimiter(20, time.Minute), h.CreateFromMessage)
	r.Post("/:id/threads", perUserLimiter(20, time.Minute), h.Create)
	r.Get("/:id/threads/archived", perUserLimiter(60, time.Minute), h.ListArchived)
	r.Get("/:id/thread", h.Get)
	r.Patch("/:id/thread", perUserLimiter(30, time.Minute), h.Update)
	r.Delete("/:id/thread", h.Delete)
	r.Get("/:id/thread-members", perUserLimiter(60, time.Minute), h.ListMembers)
	r.Put("/:id/thread-members/@me", perUserLimiter(30, time.Minute), h.Join)
	r.Delete("/:id/thread-members/@me", perUserLimiter(30, time.Minute), h.Leave)
	r.Put("/:id/thread-members/:user_id", perUserLimiter(30, time.Minute), h.AddMember)
	r.Delete("/:id/thread-members/:user_id", perUserLimiter(30, time.Minute), h.RemoveMember)

	d.App.Group("/spaces", requireAuth).Get("/:id/threads/active", perUserLimiter(60, time.Minute), h.ListActive)
}
