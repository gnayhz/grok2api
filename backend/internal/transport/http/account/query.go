package account

import (
	"errors"
	httphelpers "github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"net/http"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

func (h *Handler) list(c *gin.Context) {
	page, pageSize := httphelpers.Pagination(c)
	values, total, err := h.admin.List(c.Request.Context(), page, pageSize, c.Query("search"), accountapp.ListFilter{
		Provider: c.Query("provider"), QuotaType: c.Query("type"), Status: c.Query("status"),
		Renewal: c.Query("renewal"), Risk: c.Query("risk"), Agreement: c.Query("agreement"), Association: c.Query("association"),
		Sort: repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))},
	})
	if errors.Is(err, accountapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "accountListFailed", "读取账号失败")
		return
	}
	items := make([]accountResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newAccountResponse(value))
	}
	h.attachQualityStates(items)
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) summary(c *gin.Context) {
	value, err := h.admin.Summary(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "accountSummaryFailed", "读取账号统计失败")
		return
	}
	build := value.Providers[string(accountdomain.ProviderBuild)]
	web := value.Providers[string(accountdomain.ProviderWeb)]
	console := value.Providers[string(accountdomain.ProviderConsole)]
	response.Success(c, http.StatusOK, gin.H{
		"total": value.Total, "available": value.Available, "recovering": value.Recovering, "attention": value.Attention, "risk": value.Risk,
		"providers": gin.H{
			string(accountdomain.ProviderBuild):   gin.H{"total": build.Total, "available": build.Available},
			string(accountdomain.ProviderWeb):     gin.H{"total": web.Total, "available": web.Available},
			string(accountdomain.ProviderConsole): gin.H{"total": console.Total, "available": console.Available},
		},
		"recovery": gin.H{"cooldown": value.Recovery.Cooldown, "waitingReset": value.Recovery.WaitingReset, "probing": value.Recovery.Probing},
		"issues":   gin.H{"disabled": value.Issues.Disabled, "reauthRequired": value.Issues.ReauthRequired},
	})
}

func (h *Handler) get(c *gin.Context) {
	id, ok := httphelpers.PathID(c)
	if !ok {
		return
	}
	value, err := h.admin.Get(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountGetFailed", err, http.StatusInternalServerError, "读取账号失败")
		return
	}
	items := []accountResponse{newAccountResponse(value)}
	h.attachQualityStates(items)
	response.Success(c, http.StatusOK, items[0])
}
