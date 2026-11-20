package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountGroupOwnershipContract(t *testing.T) {
	for _, revision := range []int64{1, 7, 9007199254740991} {
		owner := AccountGroupOwnership{Version: 1, Kind: "procurement", Revision: revision}
		raw, err := json.Marshal(owner)
		require.NoError(t, err)
		var out AccountGroupOwnership
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Equal(t, owner, out)
	}
	for _, raw := range []string{
		`null`, `{}`, `[]`, `true`, `{"version":1,"kind":"manual","revision":1}`,
		`{"version":2,"kind":"procurement","revision":1}`,
		`{"version":1,"kind":"procurement","revision":0}`,
		`{"version":1,"kind":"procurement","revision":-1}`,
		`{"version":1,"kind":"procurement","revision":null}`,
		`{"version":1,"kind":"procurement","revision":"1"}`,
		`{"version":1,"kind":"procurement","revision":1.5}`,
		`{"version":1,"kind":"procurement","revision":1e2}`,
		`{"version":1,"kind":"procurement","revision":9007199254740992}`,
		`{"version":null,"kind":"procurement","revision":1}`,
		`{"version":1,"kind":"procurement","revision":1,"future":true}`,
		`{"version":1,"kind":"procurement","revision":1,"revision":2}`,
		`{"version":1,"kind":"procurement","revision":1} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			original := AccountGroupOwnership{Version: 1, Kind: "procurement", Revision: 7}
			out := original
			require.Error(t, json.Unmarshal([]byte(raw), &out))
			require.Equal(t, original, out, "rejected input must not partially erase an existing owner")
		})
	}
	for _, value := range []AccountGroupOwnership{{}, {Version: 2, Kind: "procurement", Revision: 1}, {Version: 1, Kind: "procurement"}} {
		_, err := json.Marshal(value)
		require.Error(t, err, "ORM writes must also reject invalid typed values")
	}
}
