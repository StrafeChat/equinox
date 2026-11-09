package routes

import (
	"context"

	"github.com/StrafeChat/equinox/internal/modules/media"
	"github.com/StrafeChat/equinox/internal/modules/messages"
)

// SetupMediaRoutes mounts the image proxy that serves embeds' external pictures
// (internal/modules/media) and starts its cache sweeper. Unauthenticated - an <img> cannot
// send a token, so the signature in the address is the permission - and limited per client
// address on the fetches it causes, inside the module (cached images cost nothing).
func SetupMediaRoutes(d Deps) {
	if d.Media == nil {
		return
	}
	h := media.NewHandler(d.Media, clientKey)
	d.App.Get(media.RoutePrefix+":sig/:src", h.External)
	d.Media.StartSweeper(context.Background())
}

// withMedia hands the image proxy to a messages service, so the embeds it serializes carry
// proxy URLs (and the images of the ones it writes are fetched ahead of their readers).
func withMedia(d Deps, svc *messages.Service) {
	if d.Media != nil {
		svc.SetMediaSigner(d.Media)
	}
}
