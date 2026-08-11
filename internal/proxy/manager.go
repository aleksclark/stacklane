package proxy

import (
	"context"

	"github.com/aleksclark/stacklane/internal/domain"
)

// Manager reconciles local proxy listeners for desired endpoints.
type Manager interface {
	Reconcile(ctx context.Context, eps []domain.Endpoint) error
	Shutdown(ctx context.Context) error
}
