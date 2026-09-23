package webmcpclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomToolIdentity(t *testing.T) {
	id, name := customToolIdentity("custom.ct_abcdefghijklmnopqrstuvwx.fill_payment_form")
	require.Equal(t, "ct_abcdefghijklmnopqrstuvwx", id)
	require.Equal(t, "fill_payment_form", name)

	for _, invalid := range []string{
		"custom.ct_not-a-custom-id.search",
		"custom.ct_0123456789abcdefghijklmn.search",
		"custom.ct_abcdefghijklmnopqrstuvwx_.search",
	} {
		id, name = customToolIdentity(invalid)
		require.Empty(t, id)
		require.Equal(t, invalid, name)
	}
}
