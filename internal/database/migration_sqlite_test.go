package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionedMigrationsHaveUniqueVersions(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	files, err := filepath.Glob(filepath.Join(repoRoot, "migrations", "versioned", "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	versions := make(map[string]string, len(files))
	for _, file := range files {
		name := filepath.Base(file)
		version, _, ok := strings.Cut(name, "_")
		require.Truef(t, ok, "migration %q must begin with its numeric version", name)

		previous, duplicate := versions[version]
		require.Falsef(t, duplicate, "migration version %s is used by both %s and %s", version, previous, name)
		versions[version] = name
	}
}

func TestSQLiteMigrationsIncludeAutoTagConfig(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(repoRoot))
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	dbPath := filepath.Join(t.TempDir(), "migration.db")
	require.NoError(t, RunMigrationsWithOptions("sqlite3://unused", MigrationOptions{SQLiteDBPath: dbPath}))

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.Query("PRAGMA table_info(knowledge_bases)")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		require.NoError(t, rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey))
		if name == "auto_tag_config" {
			found = true
		}
	}
	require.NoError(t, rows.Err())
	require.True(t, found, "SQLite migrations must create knowledge_bases.auto_tag_config")
}
