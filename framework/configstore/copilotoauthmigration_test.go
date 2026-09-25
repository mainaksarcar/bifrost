package configstore

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
)

func TestCopilotOAuthMigration(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&tables.TableKey{}))
	columns := []string{
		"github_copilot_auth_mode",
		"github_copilot_auth_client_id",
		"github_copilot_refresh_token",
		"github_copilot_token_expires_at",
	}
	for _, column := range columns {
		require.NoError(t, db.Migrator().DropColumn(&tables.TableKey{}, column))
	}
	require.NoError(t, migrationAddGithubCopilotOAuthColumns(context.Background(), db, testMigrationLogger))
	require.NoError(t, migrationAddGithubCopilotOAuthColumns(context.Background(), db, testMigrationLogger))
	for _, column := range columns {
		require.True(t, db.Migrator().HasColumn(&tables.TableKey{}, column))
	}
}

// A database that ran the first migration before its column list grew keeps the record of
// having applied it, so the migrator skips the wider version and the instance boots without
// the columns it now writes to. Reproduces that shape: apply the first migration, drop the
// columns it did not originally create, then confirm the follow-up restores them.
func TestCopilotRefreshMigrationBackfillsAnAlreadyMigratedDatabase(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&tables.TableKey{}))
	added := []string{
		"github_copilot_auth_client_id",
		"github_copilot_refresh_token",
		"github_copilot_token_expires_at",
	}

	require.NoError(t, migrationAddGithubCopilotOAuthColumns(context.Background(), db, testMigrationLogger))
	for _, column := range added {
		require.NoError(t, db.Migrator().DropColumn(&tables.TableKey{}, column))
		require.False(t, db.Migrator().HasColumn(&tables.TableKey{}, column))
	}
	// Re-running the first migration cannot help: it is already recorded as applied.
	require.NoError(t, migrationAddGithubCopilotOAuthColumns(context.Background(), db, testMigrationLogger))
	for _, column := range added {
		require.False(t, db.Migrator().HasColumn(&tables.TableKey{}, column),
			"the original migration is recorded as applied, so it must not be what fixes this")
	}

	require.NoError(t, migrationAddGithubCopilotRefreshColumns(context.Background(), db, testMigrationLogger))
	require.NoError(t, migrationAddGithubCopilotRefreshColumns(context.Background(), db, testMigrationLogger))
	for _, column := range added {
		require.True(t, db.Migrator().HasColumn(&tables.TableKey{}, column))
	}
	require.True(t, db.Migrator().HasColumn(&tables.TableKey{}, "github_copilot_auth_mode"))
}
