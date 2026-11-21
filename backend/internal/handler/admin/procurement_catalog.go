package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
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

// ProcurementProductImpact only reads saved state; it never enables the gate,
// sends a probe, mutates membership or publishes scheduler state.
func (h *ChannelHandler) ProcurementProductImpact(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		procurementCatalogError(c, service.ErrProcurementCatalogInvalid)
		return
	}
	var presetIDs []int64
	if values, exists := c.Request.URL.Query()["monitoring_preset_id"]; exists {
		presetID, parseErr := strconv.ParseInt(c.Query("monitoring_preset_id"), 10, 64)
		if parseErr != nil || len(values) != 1 || presetID < 1 || presetID > 9007199254740991 {
			response.ErrorFrom(c, service.ErrProcurementMonitoringRecipeInvalid)
			return
		}
		presetIDs = append(presetIDs, presetID)
	}
	repo, err := h.channelService.ProcurementCatalogRepository()
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	out, err := repo.PreviewProcurementProductImpact(ctx, id, presetIDs...)
	if err != nil {
		if errors.Is(err, service.ErrGroupNotFound) || errors.Is(err, service.ErrObserverScope) || errors.Is(err, service.ErrProcurementProductImpactTooLarge) ||
			errors.Is(err, service.ErrProcurementMonitoringRecipeInvalid) || errors.Is(err, service.ErrProcurementMonitoringRecipeNotFound) {
			response.ErrorFrom(c, err)
		} else {
			procurementCatalogError(c, err)
		}
		return
	}
	response.Success(c, out)
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
	c.Header("Cache-Control", "no-store")
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
	command, err := service.ParseProcurementPreviewCommand(raw)
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	out, err := repo.PreviewProcurementClassification(ctx, command)
	if err != nil {
		procurementCatalogError(c, err)
		return
	}
	response.Success(c, out)
}

// First-time additions only. Existing plans and product ownership are untouched.
func (h *ChannelHandler) ApplyProcurementRecipe(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 || id > 9007199254740991 {
		response.ErrorFrom(c, service.ErrProcurementRecipeCommand)
		return
	}
	repo, err := h.channelService.ProcurementRecipeRepository()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	if err != nil {
		response.ErrorFrom(c, service.ErrProcurementRecipeCommand)
		return
	}
	command, err := service.ParseProcurementRecipeCommand(raw)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	middleware.SetAuditAction(c, "admin.upstream_procurement.monitoring_recipe")
	middleware.SetAuditExtra(c, map[string]any{"group_id": id, "preset_id": command.PresetID})
	out, err := repo.ApplyProcurementRecipe(ctx, id, command)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrProcurementRecipeCommand), errors.Is(err, service.ErrProcurementRecipeConflict), errors.Is(err, service.ErrGroupNotFound), errors.Is(err, service.ErrObserverScope), errors.Is(err, service.ErrProcurementProductImpactTooLarge):
			response.ErrorFrom(c, err)
		default:
			response.ErrorFrom(c, service.ErrProcurementRecipeUnavailable)
		}
		return
	}
	response.Success(c, out)
}

// Separate authorization from price simulation and automatic execution. Error
// responses never expose stored configuration, SQL details or credentials.
func (h *ChannelHandler) ProcurementManagement(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 || id > 9007199254740991 {
		response.ErrorFrom(c, service.ErrProcurementManagementInvalid)
		return
	}
	repo, err := h.channelService.ProcurementManagementRepository()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	respondError := func(err error) {
		switch {
		case errors.Is(err, service.ErrProcurementManagementInvalid), errors.Is(err, service.ErrProcurementManagementConflict),
			errors.Is(err, service.ErrGroupNotFound), errors.Is(err, service.ErrObserverScope), errors.Is(err, service.ErrProcurementProductImpactTooLarge):
			response.ErrorFrom(c, err)
		default:
			response.ErrorFrom(c, service.ErrProcurementManagementUnavailable)
		}
	}
	if c.Request.Method == http.MethodGet {
		out, err := repo.ReadProcurementManagement(ctx, id)
		if err != nil {
			respondError(err)
			return
		}
		response.Success(c, out)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 65536))
	if err != nil {
		respondError(service.ErrProcurementManagementInvalid)
		return
	}
	if c.Request.Method == http.MethodPost {
		ids, err := service.ParseProcurementManagementPreview(raw)
		if err != nil {
			respondError(err)
			return
		}
		out, err := repo.PreviewProcurementManagement(ctx, id, ids)
		if err != nil {
			respondError(err)
			return
		}
		response.Success(c, out)
		return
	}
	command, err := service.ParseProcurementManagementCommand(raw)
	if err != nil {
		respondError(err)
		return
	}
	middleware.SetAuditAction(c, "admin.upstream_procurement.management")
	middleware.SetAuditExtra(c, map[string]any{"group_id": id, "mode": command.Mode, "expected_revision": command.ExpectedRevision, "selected_count": len(command.AccountIDs)})
	out, err := repo.SaveProcurementManagement(ctx, id, command)
	if err != nil {
		respondError(err)
		return
	}
	response.Success(c, out)
}
