package domain

import (
	"encoding/json"
	"errors"
)

// ProcurementManagement authorizes review of existing memberships only. It is
// not the procurement price policy, an eligibility verdict or an auto executor.
// SQL NULL denotes the original manual product, revision zero.
type ProcurementManagement struct {
	Version  int    `json:"version"`
	Revision int64  `json:"revision"`
	Mode     string `json:"mode"`
}

func (m ProcurementManagement) Validate() error {
	if m.Version != 1 || m.Revision < 1 || m.Revision > MaxProcurementPolicyRevision || (m.Mode != "manual" && m.Mode != "review") {
		return errors.New("invalid procurement management")
	}
	return nil
}

func (m *ProcurementManagement) UnmarshalJSON(raw []byte) error {
	if _, err := procurementJSONFields(raw, []string{"version", "revision", "mode"}); err != nil {
		return err
	}
	type contract ProcurementManagement
	var next contract
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	value := ProcurementManagement(next)
	if err := value.Validate(); err != nil {
		return err
	}
	*m = value
	return nil
}

func (m ProcurementManagement) MarshalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	type contract ProcurementManagement
	return json.Marshal(contract(m))
}
