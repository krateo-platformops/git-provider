package option

import (
	"time"

	"github.com/krateo-platformops/provider-runtime/pkg/controller"
)

type GitOptions struct {
	CommitAuthorName  string
	CommitAuthorEmail string
	HomeDir           string
}

type ControllerOptions struct {
	controller.Options
	Timeout time.Duration
}

type SetupOptions struct {
	Controller ControllerOptions
	Git        GitOptions
}
