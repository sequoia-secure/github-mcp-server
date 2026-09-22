package context

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInstallationOwner(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		login, ok := GetInstallationOwner(context.Background())
		assert.False(t, ok)
		assert.Empty(t, login)
	})

	t.Run("round trip", func(t *testing.T) {
		ctx := WithInstallationOwner(context.Background(), "acme")
		login, ok := GetInstallationOwner(ctx)
		assert.True(t, ok)
		assert.Equal(t, "acme", login)
	})

	t.Run("empty login reads as absent", func(t *testing.T) {
		ctx := WithInstallationOwner(context.Background(), "")
		_, ok := GetInstallationOwner(ctx)
		assert.False(t, ok)
	})

	t.Run("inner value wins", func(t *testing.T) {
		ctx := WithInstallationOwner(WithInstallationOwner(context.Background(), "acme"), "beta")
		login, _ := GetInstallationOwner(ctx)
		assert.Equal(t, "beta", login)
	})

	t.Run("does not collide with other context values", func(t *testing.T) {
		ctx := WithGraphQLFeatures(WithInstallationOwner(context.Background(), "acme"), "f1")
		login, ok := GetInstallationOwner(ctx)
		assert.True(t, ok)
		assert.Equal(t, "acme", login)
		assert.Equal(t, []string{"f1"}, GetGraphQLFeatures(ctx))
	})
}
