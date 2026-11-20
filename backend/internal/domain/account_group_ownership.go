package domain

import (
	"encoding/json"
	"errors"
)

// AccountGroupOwnership is stored on the original membership, not in a second
// account inventory. SQL NULL means a manual/legacy membership. A revision names
// the explicit product authorization that claimed this particular relationship.
type AccountGroupOwnership struct {
	Version  int    `json:"version"`
	Kind     string `json:"kind"`
	Revision int64  `json:"revision"`
}

func (o AccountGroupOwnership) Validate() error {
	if o.Version != 1 || o.Kind != "procurement" || o.Revision < 1 || o.Revision > 9007199254740991 {
		return errors.New("invalid account group ownership")
	}
	return nil
}

func (o *AccountGroupOwnership) UnmarshalJSON(raw []byte) error {
	if _, err := procurementJSONFields(raw, []string{"version", "kind", "revision"}); err != nil {
		return err
	}
	type contract AccountGroupOwnership
	var next contract
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	value := AccountGroupOwnership(next)
	if err := value.Validate(); err != nil {
		return err
	}
	*o = value
	return nil
}

func (o AccountGroupOwnership) MarshalJSON() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	type contract AccountGroupOwnership
	return json.Marshal(contract(o))
}
