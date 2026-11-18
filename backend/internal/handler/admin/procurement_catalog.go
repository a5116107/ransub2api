package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func procurementCatalogError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrProcurementCatalogInvalid) || errors.Is(err, service.ErrProcurementCatalogConflict) {
		response.ErrorFrom(c, err)
		return
	}
	// Never forward SQL errors, account credentials or invalid stored JSON.
	response.ErrorFrom(c, service.ErrProcurementCatalogUnavailable)
}

func (h *ChannelHandler) ProcurementCatalog(c *gin.Context) {
	repo, err := h.channelService.ProcurementCatalogRepository()
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	if c.Request.Method == http.MethodGet {
		catalog, err := repo.ReadProcurementCatalog(ctx)
		if err != nil {
			procurementCatalogError(c, err)
			return
		}
		response.Success(c, service.ProcurementCatalogSnapshot{Catalog: catalog, Gaps: catalog.Gaps()})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 65536))
	if err != nil {
		procurementCatalogError(c, service.ErrProcurementCatalogInvalid)
		return
	}
	draft, err := service.ParseProcurementCatalogCommand(raw)
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	middleware.SetAuditAction(c, "admin.upstream_procurement.catalog")
	middleware.SetAuditExtra(c, map[string]any{"config_version": draft.Revision, "bucket_count": len(draft.Buckets)})
	catalog, err := repo.SaveProcurementCatalog(ctx, draft)
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	response.Success(c, service.ProcurementCatalogSnapshot{Catalog: catalog, Gaps: catalog.Gaps()})
}

func (h *ChannelHandler) ProcurementClassificationPreview(c *gin.Context) {
	repo, err := h.channelService.ProcurementCatalogRepository()
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 65536))
	if err != nil {
		procurementCatalogError(c, service.ErrProcurementCatalogInvalid)
		return
	}
	requests, err := service.ParseProcurementPreviewCommand(raw)
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	out, err := repo.PreviewProcurementClassification(ctx, requests)
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	response.Success(c, out)
}
