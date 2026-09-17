package account

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

func (h *Handler) list(c *gin.Context) {
	page, pageSize := httphelpers.Pagination(c)
	values, total, err := h.admin.List(c.Request.Context(), page, pageSize, c.Query("search"), accountapp.ListFilter{
		Quality: c.Query("quality"), Provider: c.Query("provider"), QuotaType: c.Query("type"), Status: c.Query("status"),
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
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

// identities resolves explicit references; it never enumerates the account pool.
func (h *Handler) identities(c *gin.Context) {
	parts := strings.Split(c.Query("ids"), ",")
	if len(parts) > 500 {
		response.Error(c, 400, "invalidIds", "单次最多查询 500 个账号")
		return
	}
	ids := make([]uint64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseUint(part, 10, 64)
		if err != nil || id == 0 {
			response.Error(c, 400, "invalidIds", "账号 ID 无效")
			return
		}
		ids = append(ids, id)
	}
	values, err := h.admin.Identities(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "accountIdentitiesFailed", err, 500, "读取账号名称失败")
		return
	}
	type identityResponse struct {
		ID       uint64 `json:"id,string"`
		Name     string `json:"name"`
		Email    string `json:"email"`
		Provider string `json:"provider"`
	}
	items := make([]identityResponse, 0, len(values))
	for _, value := range values {
		items = append(items, identityResponse{value.ID, value.Name, value.Email, string(value.Provider)})
	}
	response.Success(c, 200, gin.H{"items": items})
}
