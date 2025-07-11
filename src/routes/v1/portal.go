package routes_v1

import (
	"log"
	"net/http"
	"os"

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
	fasthttpadaptor.ConvertRequest(c.Context(), request, true)

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
	switch event.GetEvent() {
	case "participant_joined":
		log.Println("participant joined ", event.GetParticipant().Identity)
	}
}
