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
