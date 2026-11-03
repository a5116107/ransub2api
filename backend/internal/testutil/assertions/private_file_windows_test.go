package assertions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/securefile"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestPrivateFileRequiresProtectedOwnerOnlyWindowsACL(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		sddl  string
		flags windows.SECURITY_INFORMATION
	}{
		{"world access", "D:P(A;;FA;;;WD)", windows.PROTECTED_DACL_SECURITY_INFORMATION},
		{"null DACL", "D:NO_ACCESS_CONTROL", windows.PROTECTED_DACL_SECURITY_INFORMATION},
		{"inherited DACL", "D:(A;;FA;;;" + user.User.Sid.String() + ")", windows.UNPROTECTED_DACL_SECURITY_INFORMATION},
		{"insufficient owner rights", "D:P(A;;FR;;;" + user.User.Sid.String() + ")", windows.PROTECTED_DACL_SECURITY_INFORMATION},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private")
			require.NoError(t, securefile.WriteAtomic(path, []byte("private")))
			PrivateFile(t, path)
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			require.NoError(t, err)
			dacl, _, err := sd.DACL()
			require.NoError(t, err)
			require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
				windows.DACL_SECURITY_INFORMATION|tc.flags, nil, nil, dacl, nil))
			require.Error(t, privateFileError(path), "readable-by-others or inherited ACLs are not equivalent to 0600")
			f, err := os.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })
			require.Error(t, securefile.CheckPrivate(f), "production key reads must reject the same exposed ACLs")
		})
	}
}
