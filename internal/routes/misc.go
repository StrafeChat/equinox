package routes

import (
	"github.com/StrafeChat/equinox/internal/modules/misc"
)

func SetupMiscRoutes(d Deps) {
	h := misc.NewHandler(d.Config)

	d.App.Get("/", h.Index)
}
