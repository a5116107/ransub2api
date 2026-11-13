//go:build !windows

package basispoints

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Windows FileMode does not represent an ACL. Keep the POSIX privacy contract
// here; content, expiry and quota cleanup remain covered on every platform.
func TestImageRelayPrivatePOSIXPermissions(t *testing.T) {
	r, err := newTestImageRelay(t, "https://images.example")
	require.NoError(t, err)
	out, err := r.Rewrite(relayTestRequest(t, relayTestPNG(t)), "scope")
	require.NoError(t, err)
	token := strings.TrimPrefix(relayTestURL(t, out), r.baseURL+ImageRelayPath)
	img := r.entries[token]
	require.NotNil(t, img)
	info, err := os.Stat(img.path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	info, err = os.Stat(r.dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
}
