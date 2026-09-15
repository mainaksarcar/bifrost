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
	columns := []string{"github_copilot_auth_mode", "github_copilot_o_auth_client_id"}
	for _, column := range columns {
		require.NoError(t, db.Migrator().DropColumn(&tables.TableKey{}, column))
	}
	require.NoError(t, migrationAddGithubCopilotOAuthColumns(context.Background(), db, testMigrationLogger))
	require.NoError(t, migrationAddGithubCopilotOAuthColumns(context.Background(), db, testMigrationLogger))
	for _, column := range columns {
		require.True(t, db.Migrator().HasColumn(&tables.TableKey{}, column))
	}
}
