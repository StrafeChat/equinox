package routes_v1

import (
	"net/http"
	"os"

	"github.com/StrafeChat/equinox/src/portal"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"

	"github.com/gofiber/fiber/v3"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/webhook"
)

var (
	apiKey    = os.Getenv("LIVEKIT_API_KEY")
	apiSecret = os.Getenv("LIVEKIT_API_SECRET")
)

func SetupPortalRoutes(versionRouter *fiber.Group) {
	router := versionRouter.Group("/portal")

	router.Post("", processRequest)
}

func processRequest(c fiber.Ctx) error {
	request := &http.Request{}
	fasthttpadaptor.ConvertRequest(c.Context().(*fasthttp.RequestCtx), request, true)

	ServeHTTP(request)

	return nil
}

func ServeHTTP(r *http.Request) {
	authProvider := auth.NewSimpleKeyProvider(
		apiKey, apiSecret,
	)
	// Event is a livekit.WebhookEvent{} object
	event, err := webhook.ReceiveWebhookEvent(r, authProvider)
	if err != nil {
		// Could not validate, handle error
		return
	}

	// Consume WebhookEvent
	portal.ProcessWebhookEvent(event)
}
