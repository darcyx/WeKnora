package service_test

import (
	"context"
	"io"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestStorageBackendResolverScopesPathsAndTenant(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.StorageBackend{}))
	repo := repository.NewStorageBackendRepository(db)
	backend := &types.StorageBackend{TenantID: 7, Name: "Local A", Provider: "local", Config: types.StorageBackendConfig{}, LegacyAlias: true}
	require.NoError(t, repo.Create(context.Background(), backend))

	resolver := service.NewStorageBackendService(repo, db)
	svc, provider, err := resolver.ResolveFileService(context.Background(), &types.Tenant{ID: 7}, backend.ID, "local", t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "local", provider)

	path, err := svc.SaveBytes(context.Background(), []byte("scoped"), 7, "exports/a.txt", false)
	require.NoError(t, err)
	assert.Contains(t, path, "storage://"+backend.ID+"/local://")
	reader, err := svc.GetFile(context.Background(), path)
	require.NoError(t, err)
	defer reader.Close()
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "scoped", string(data))

	_, _, err = resolver.ResolveFileService(context.Background(), &types.Tenant{ID: 8}, backend.ID, "local", t.TempDir())
	require.Error(t, err)
}

func TestStorageBackendResolverResolvesResourceBackend(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.StorageBackend{}, &types.StoredResource{}))

	backendRepo := repository.NewStorageBackendRepository(db)
	resourceRepo := repository.NewResourceRepository(db)
	resourceCatalog := service.NewResourceCatalog(resourceRepo)
	backend := &types.StorageBackend{TenantID: 7, Name: "Local A", Provider: "local", Config: types.StorageBackendConfig{}, LegacyAlias: true}
	require.NoError(t, backendRepo.Create(context.Background(), backend))

	resolver := service.NewStorageBackendServiceWithResources(backendRepo, db, resourceCatalog)
	physical := types.BuildStorageBackendPath(backend.ID, "local://7/exports/image.png")
	ref, err := resourceCatalog.Register(context.Background(), 7, physical, interfaces.ResourceRegistration{Kind: "image"})
	require.NoError(t, err)

	svc, err := resolver.ResolveResourceFileService(context.Background(), ref, t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, svc)

	got, err := svc.GetFileURL(context.Background(), ref)
	require.NoError(t, err)
	assert.Equal(t, physical, got, "resource URL resolution must preserve the scoped backend path")
}
