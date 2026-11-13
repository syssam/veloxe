package integration_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/syssam/velox"
	"github.com/syssam/velox/dialect/sql"
	"github.com/syssam/velox/privacy"
	"github.com/syssam/velox/runtime"
	integration "github.com/syssam/velox/tests/integration"
)

// The Ent soft-delete pattern on velox's split packages: a hook turns a delete
// into an update and re-runs it with runtime.FindMutator and the mutation's own
// Config(), where Ent calls mx.Client().Mutate. User.nickname stands in for a
// deleted_at column (Optional, Nillable).

type hardDeleteKey struct{}

func hardDelete(ctx context.Context) context.Context {
	return context.WithValue(ctx, hardDeleteKey{}, true)
}

func softDeleteNickname(next integration.Mutator) integration.Mutator {
	return integration.MutateFunc(func(ctx context.Context, m integration.Mutation) (integration.Value, error) {
		if !m.Op().Is(velox.OpDelete|velox.OpDeleteOne) || ctx.Value(hardDeleteKey{}) != nil {
			return next.Mutate(ctx, m)
		}
		mx, ok := m.(interface {
			SetOp(runtime.Op)
			Config() runtime.Config
			Filter() privacy.Filter
			SetNickname(string)
		})
		if !ok {
			return next.Mutate(ctx, m)
		}
		mx.Filter().WhereP(sql.FieldIsNull("nickname"))
		mx.SetNickname("deleted")
		mx.SetOp(velox.OpUpdate)
		return runtime.FindMutator(m.Type())(ctx, mx.Config(), m)
	})
}

func TestSoftDeleteHook_DeleteBecomesUpdate(t *testing.T) {
	client := openTestClient(t)
	client.User.Use(softDeleteNickname)
	ctx := context.Background()

	a := createUser(t, client, "A", "a@test.com")
	b := createUser(t, client, "B", "b@test.com")

	// DeleteOneID: the row stays, marked.
	require.NoError(t, client.User.DeleteOneID(a.ID).Exec(ctx))
	got, err := client.User.Get(ctx, a.ID)
	require.NoError(t, err, "the row must survive a soft delete")
	require.NotNil(t, got.Nickname)
	assert.Equal(t, "deleted", *got.Nickname)

	// A second delete of the same row changes nothing: the guard keeps the
	// first mark, and DeleteOne reports not-found as for a missing row.
	err = client.User.DeleteOneID(a.ID).Exec(ctx)
	require.Error(t, err)
	assert.True(t, velox.IsNotFound(err), "got %v", err)

	// Bulk delete: the count is the rows marked, and already-marked rows are
	// not counted again.
	n, err := client.User.Delete().Exec(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only B was live")
	got, err = client.User.Get(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Nickname)
	assert.Equal(t, "deleted", *got.Nickname)

	total, err := client.User.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, total, "no row was removed")
}

// The re-run must use the transaction's driver: rolled back, the mark is gone.
func TestSoftDeleteHook_RunsInTheCallersTransaction(t *testing.T) {
	client := openTestClient(t)
	client.User.Use(softDeleteNickname)
	ctx := context.Background()
	u := createUser(t, client, "T", "t@test.com")

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.User.DeleteOneID(u.ID).Exec(ctx))
	require.NoError(t, tx.Rollback())

	got, err := client.User.Get(ctx, u.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Nickname, "a rolled-back soft delete leaves no mark")

	tx, err = client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.User.DeleteOneID(u.ID).Exec(ctx))
	require.NoError(t, tx.Commit())

	got, err = client.User.Get(ctx, u.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Nickname)
	assert.Equal(t, "deleted", *got.Nickname)
}

// Under the opt-out the hook passes the delete on: the row is removed.
func TestSoftDeleteHook_OptOutRemovesTheRow(t *testing.T) {
	client := openTestClient(t)
	client.User.Use(softDeleteNickname)
	ctx := context.Background()
	u := createUser(t, client, "H", "h@test.com")

	require.NoError(t, client.User.DeleteOneID(u.ID).Exec(hardDelete(ctx)))
	_, err := client.User.Get(ctx, u.ID)
	assert.True(t, velox.IsNotFound(err), "got %v", err)
}
