package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func procurementSourceMappingError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrProcurementSourceMappingInvalid) || errors.Is(err, service.ErrProcurementSourceMappingConflict) ||
		errors.Is(err, service.ErrProcurementSourceMappingNotFound) || errors.Is(err, service.ErrObserverScope) {
		response.ErrorFrom(c, err)
		return
	}
	response.ErrorFrom(c, service.ErrProcurementSourceMappingUnavailable)
}

func (h *ChannelHandler) ProcurementSourceMappings(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	repo, err := h.channelService.ProcurementSourceMappingRepository()
	if err != nil {
		procurementSourceMappingError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	switch c.Request.Method {
	case http.MethodGet:
		if c.Param("id") != "" {
			id, err := strconv.ParseInt(c.Param("id"), 10, 64)
			if err != nil {
				procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
				return
			}
			out, err := repo.GetProcurementSourceMapping(ctx, id)
			if err != nil {
				procurementSourceMappingError(c, err)
				return
			}
			h.channelService.AddProcurementSourcePricing(out)
			response.Success(c, out)
			return
		}
		page, size := response.ParsePagination(c)
		out, err := repo.ListProcurementSourceMappings(ctx, c.Query("search"), c.Query("platform"), page, size)
		if err != nil {
			procurementSourceMappingError(c, err)
			return
		}
		for i := range out.Items {
			h.channelService.AddProcurementSourcePricing(&out.Items[i])
		}
		response.Success(c, out)
	case http.MethodPost:
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 16384))
		if err != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		var command domain.ProcurementSourceMappingCommand
		if json.Unmarshal(raw, &command) != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		middleware.SetAuditAction(c, "admin.upstream_procurement.source_mapping.create")
		middleware.SetAuditExtra(c, map[string]any{"source": "procurement_source_mapping", "platform": command.Platform, "site_code": command.SiteCode, "local_group_id": command.LocalGroupID, "account_id": command.AccountID})
		out, err := repo.CreateProcurementSourceMapping(ctx, command)
		if err != nil {
			procurementSourceMappingError(c, err)
			return
		}
		middleware.SetAuditExtra(c, map[string]any{"source": "procurement_source_mapping", "mapping_id": out.ID, "source_group_id": out.SourceGroupID, "revision": out.Revision})
		response.Success(c, out)
	case http.MethodPut:
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id < 1 {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
		if err != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		var command domain.ProcurementSourceMappingEnableCommand
		if json.Unmarshal(raw, &command) != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		middleware.SetAuditAction(c, "admin.upstream_procurement.source_mapping.set_enabled")
		middleware.SetAuditExtra(c, map[string]any{"source": "procurement_source_mapping", "mapping_id": id, "revision": command.ExpectedRevision, "enabled": command.Enabled})
		out, err := repo.SetProcurementSourceMappingEnabled(ctx, id, command.ExpectedRevision, command.Enabled, command.Confirm)
		if err != nil {
			procurementSourceMappingError(c, err)
			return
		}
		response.Success(c, out)
	case http.MethodDelete:
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id < 1 {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
		if err != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		var command domain.ProcurementSourceMappingDeleteCommand
		if json.Unmarshal(raw, &command) != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		middleware.SetAuditAction(c, "admin.upstream_procurement.source_mapping.delete")
		middleware.SetAuditExtra(c, map[string]any{"source": "procurement_source_mapping", "mapping_id": id, "revision": command.ExpectedRevision})
		if err := repo.DeleteProcurementSourceMapping(ctx, id, command.ExpectedRevision, command.Confirm); err != nil {
			procurementSourceMappingError(c, err)
			return
		}
		response.Success(c, gin.H{"deleted": id})
	default:
		procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
	}
}

func (h *ChannelHandler) ProcurementSourceGroup(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	repo, err := h.channelService.ProcurementSourceMappingRepository()
	if err != nil {
		procurementSourceMappingError(c, err)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	if err != nil {
		procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
		return
	}
	var command domain.ProcurementSourceMappingEnableCommand
	if json.Unmarshal(raw, &command) != nil {
		procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	middleware.SetAuditAction(c, "admin.upstream_procurement.source_group.set_enabled")
	middleware.SetAuditExtra(c, map[string]any{"source": "procurement_source_mapping", "source_group_id": id, "revision": command.ExpectedRevision, "enabled": command.Enabled})
	out, err := repo.SetProcurementSourceGroupEnabled(ctx, id, command.ExpectedRevision, command.Enabled, command.Confirm)
	if err != nil {
		procurementSourceMappingError(c, err)
		return
	}
	response.Success(c, out)
}

func (h *ChannelHandler) ProcurementSourceSite(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	repo, err := h.channelService.ProcurementSourceMappingRepository()
	if err != nil {
		procurementSourceMappingError(c, err)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
		return
	}
	if c.Request.Method == http.MethodPut {
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
		if err != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		var command domain.ProcurementSupplierSiteRenameCommand
		if json.Unmarshal(raw, &command) != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		middleware.SetAuditAction(c, "admin.upstream_procurement.source_site.rename")
		middleware.SetAuditExtra(c, map[string]any{"source": "procurement_source_mapping", "site_id": id, "revision": command.ExpectedRevision})
		out, err := repo.RenameProcurementSourceSite(ctx, id, command)
		if err != nil {
			procurementSourceMappingError(c, err)
			return
		}
		response.Success(c, out)
		return
	}
	if c.Request.Method == http.MethodDelete {
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
		if err != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		var command domain.ProcurementSourceMappingDeleteCommand
		if json.Unmarshal(raw, &command) != nil {
			procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		middleware.SetAuditAction(c, "admin.upstream_procurement.source_site.delete")
		middleware.SetAuditExtra(c, map[string]any{"source": "procurement_source_mapping", "site_id": id, "revision": command.ExpectedRevision})
		if err := repo.DeleteProcurementSourceSite(ctx, id, command.ExpectedRevision, command.Confirm); err != nil {
			procurementSourceMappingError(c, err)
			return
		}
		response.Success(c, gin.H{"deleted": id})
		return
	}
	procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
}

func (h *ChannelHandler) DeleteProcurementSourceGroup(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	repo, err := h.channelService.ProcurementSourceMappingRepository()
	if err != nil {
		procurementSourceMappingError(c, err)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	if err != nil {
		procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
		return
	}
	var command domain.ProcurementSourceMappingDeleteCommand
	if json.Unmarshal(raw, &command) != nil {
		procurementSourceMappingError(c, service.ErrProcurementSourceMappingInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	middleware.SetAuditAction(c, "admin.upstream_procurement.source_group.delete")
	middleware.SetAuditExtra(c, map[string]any{"source": "procurement_source_mapping", "source_group_id": id, "revision": command.ExpectedRevision})
	if err := repo.DeleteProcurementSourceGroup(ctx, id, command.ExpectedRevision, command.Confirm); err != nil {
		procurementSourceMappingError(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": id})
}
