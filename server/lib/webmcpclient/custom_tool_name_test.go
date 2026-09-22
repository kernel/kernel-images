package webmcpclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomToolIdentity(t *testing.T) {
	id, name := customToolIdentity("custom.ct_0123456789abcdef.fill_payment_form")
	require.Equal(t, "ct_0123456789abcdef", id)
	require.Equal(t, "fill_payment_form", name)

	id, name = customToolIdentity("custom.ct_not-a-custom-id.search")
	require.Empty(t, id)
	require.Equal(t, "custom.ct_not-a-custom-id.search", name)
}
