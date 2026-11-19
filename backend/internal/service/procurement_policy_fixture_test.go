package service

// Shared fixtures must compile with and without the unit build tag.

import (
	"github.com/Wei-Shaw/sub2api/internal/domain"
)

func procurementBucketTestCatalog() *domain.ProcurementCatalog {
	bound := func(s string) *string { return &s }
	return &domain.ProcurementCatalog{Version: 1, Revision: 1, Buckets: []domain.ProcurementBucket{
		{Code: "low", Name: "Low", MinRate: bound("0.02"), MaxRate: bound("0.03")},
		{Code: "next", Name: "Next", MinRate: bound("0.03"), MaxRate: bound("0.05")},
	}}
}

func procurementSourceGroupTestPolicy() *ProcurementPolicy {
	return &ProcurementPolicy{Version: 6, Revision: 5, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{}, Quality: "not_required", Availability: "not_required", SourceGroupIDs: []int64{700}}
}
func procurementSourceGroupTestSnapshot(groupID int64, ids []int64) *domain.ProcurementSourceSnapshot {
	return &domain.ProcurementSourceSnapshot{Version: 1, ProductGroupID: groupID, PolicyRevision: 5, GroupIDs: []int64{700}, AccountIDs: ids}
}
