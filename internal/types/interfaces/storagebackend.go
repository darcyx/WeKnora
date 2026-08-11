package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

type StorageBackendRepository interface {
	Create(ctx context.Context, backend *types.StorageBackend) error
	GetByID(ctx context.Context, tenantID uint64, id string) (*types.StorageBackend, error)
	List(ctx context.Context, tenantID uint64) ([]*types.StorageBackend, error)
	Update(ctx context.Context, backend *types.StorageBackend) error
	Delete(ctx context.Context, tenantID uint64, id string) error
	FindLegacyAlias(ctx context.Context, tenantID uint64, provider string) (*types.StorageBackend, error)
}

type StorageBackendService interface {
	Create(ctx context.Context, backend *types.StorageBackend) error
	Update(ctx context.Context, backend *types.StorageBackend) error
	Delete(ctx context.Context, tenantID uint64, id string) error
	SetDefault(ctx context.Context, tenantID uint64, id string) error
	Test(ctx context.Context, backend *types.StorageBackend) error
}

// StorageBackendResolver is the single runtime entry point for resolving one
// concrete storage instance. backendID wins; provider is a legacy fallback.
type StorageBackendResolver interface {
	ResolveFileService(ctx context.Context, tenant *types.Tenant, backendID, provider, localBaseDir string) (FileService, string, error)
	ResolveBackend(ctx context.Context, tenant *types.Tenant, backendID, provider string) (*types.StorageBackend, error)
}

// ResourceFileServiceResolver is an optional extension for resolving a stable
// resource:// handle through the storage backend recorded on that resource.
// It is kept separate from StorageBackendResolver so focused implementations
// that only resolve provider paths do not need to know about the resource catalog.
type ResourceFileServiceResolver interface {
	ResolveResourceFileService(ctx context.Context, reference, localBaseDir string) (FileService, error)
}
