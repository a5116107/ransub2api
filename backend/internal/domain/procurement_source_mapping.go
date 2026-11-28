package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ProcurementSourceMappingVersion = 1

var procurementSourceCode = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ProcurementSourceMappingCommand explicitly relates an upstream supplier
// group to a local source Group and Account route. It is only a declaration;
// it never proves group selection, model identity, quality, or dispatchability.
type ProcurementSourceMappingCommand struct {
	Version           int    `json:"version"`
	SiteCode          string `json:"site_code"`
	SiteName          string `json:"site_name"`
	Platform          string `json:"platform"`
	ExternalGroupID   string `json:"external_group_id"`
	ExternalGroupName string `json:"external_group_name"`
	LocalGroupID      int64  `json:"local_group_id"`
	AccountID         int64  `json:"account_id"`
	Enabled           *bool  `json:"enabled,omitempty"`
	Confirm           bool   `json:"confirm"`
}

type ProcurementSourceMappingEnableCommand struct {
	Version          int   `json:"version"`
	ExpectedRevision int64 `json:"expected_revision"`
	Enabled          bool  `json:"enabled"`
	Confirm          bool  `json:"confirm"`
}

func (c *ProcurementSourceMappingEnableCommand) UnmarshalJSON(raw []byte) error {
	fields, err := procurementJSONFields(raw, []string{"version", "expected_revision", "enabled", "confirm"})
	if err != nil {
		return err
	}
	for _, name := range []string{"version", "expected_revision", "enabled", "confirm"} {
		if bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
			return errors.New("null source mapping state is not supported")
		}
	}
	type command ProcurementSourceMappingEnableCommand
	var next command
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	*c = ProcurementSourceMappingEnableCommand(next)
	if c.Version != ProcurementSourceMappingVersion || c.ExpectedRevision < 1 || c.ExpectedRevision >= MaxProcurementPolicyRevision || !c.Confirm {
		return errors.New("invalid procurement source mapping state command")
	}
	return nil
}

type ProcurementSourceMappingDeleteCommand struct {
	Version          int   `json:"version"`
	ExpectedRevision int64 `json:"expected_revision"`
	Confirm          bool  `json:"confirm"`
}

type ProcurementSupplierSiteRenameCommand struct {
	Version          int    `json:"version"`
	ExpectedRevision int64  `json:"expected_revision"`
	Name             string `json:"name"`
	Confirm          bool   `json:"confirm"`
}

func (c *ProcurementSupplierSiteRenameCommand) UnmarshalJSON(raw []byte) error {
	fields, err := procurementJSONFields(raw, []string{"version", "expected_revision", "name", "confirm"})
	if err != nil {
		return err
	}
	for _, name := range []string{"version", "expected_revision", "name", "confirm"} {
		if bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
			return errors.New("null supplier site rename fields are not supported")
		}
	}
	type command ProcurementSupplierSiteRenameCommand
	var next command
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	*c = ProcurementSupplierSiteRenameCommand(next)
	return c.Validate()
}

func (c ProcurementSupplierSiteRenameCommand) Validate() error {
	if c.Version != ProcurementSourceMappingVersion || c.ExpectedRevision < 1 || c.ExpectedRevision >= MaxProcurementPolicyRevision ||
		!validSourceLabel(c.Name, 100, false) || !c.Confirm {
		return errors.New("invalid supplier site rename command")
	}
	return nil
}

func (c *ProcurementSourceMappingDeleteCommand) UnmarshalJSON(raw []byte) error {
	fields, err := procurementJSONFields(raw, []string{"version", "expected_revision", "confirm"})
	if err != nil {
		return err
	}
	for _, name := range []string{"version", "expected_revision", "confirm"} {
		if bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
			return errors.New("null source mapping deletion is not supported")
		}
	}
	type command ProcurementSourceMappingDeleteCommand
	var next command
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	*c = ProcurementSourceMappingDeleteCommand(next)
	if c.Version != ProcurementSourceMappingVersion || c.ExpectedRevision < 1 || c.ExpectedRevision >= MaxProcurementPolicyRevision || !c.Confirm {
		return errors.New("invalid procurement source mapping deletion command")
	}
	return nil
}

func (c *ProcurementSourceMappingCommand) UnmarshalJSON(raw []byte) error {
	fields, err := procurementJSONFields(raw,
		[]string{"version", "site_code", "site_name", "platform", "external_group_id", "local_group_id", "account_id", "confirm"},
		"external_group_name", "enabled")
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(fields["external_group_name"]), []byte("null")) ||
		bytes.Equal(bytes.TrimSpace(fields["enabled"]), []byte("null")) {
		return errors.New("null source mapping fields are not supported")
	}
	type command ProcurementSourceMappingCommand
	var next command
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	*c = ProcurementSourceMappingCommand(next)
	return c.Validate()
}

func (c ProcurementSourceMappingCommand) Validate() error {
	if c.Version != ProcurementSourceMappingVersion || !c.Confirm || !procurementSourceCode.MatchString(c.SiteCode) ||
		!validSourceLabel(c.SiteName, 100, false) || !IsConcretePlatform(c.Platform) ||
		!validSourceLabel(c.ExternalGroupID, 256, false) || !validSourceLabel(c.ExternalGroupName, 100, true) ||
		c.LocalGroupID < 1 || c.LocalGroupID > MaxProcurementPolicyRevision || c.AccountID < 1 || c.AccountID > MaxProcurementPolicyRevision {
		return errors.New("invalid procurement source mapping")
	}
	return nil
}

func validSourceLabel(value string, max int, optional bool) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) != value || (!optional && value == "") || utf8.RuneCountInString(value) > max {
		return false
	}
	return strings.IndexFunc(value, unicode.IsControl) < 0
}
