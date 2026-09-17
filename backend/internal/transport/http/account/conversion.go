package account

import (
	"net/http"
	"sync/atomic"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

type buildConversionRequest struct {
	IDs      []string                           `json:"ids"`
	All      bool                               `json:"all"`
	Strategy accountapp.BuildConversionStrategy `json:"strategy"`
}

// webConsoleSyncRequest 与 buildConversionRequest 同族：all 与 ids 由
// 应用层互斥校验，strategy 由应用层枚举校验。
type webConsoleSyncRequest struct {
	IDs      []string                          `json:"ids"`
	All      bool                              `json:"all"`
	Strategy accountapp.WebConsoleSyncStrategy `json:"strategy"`
}

type buildConversionResponse struct {
	Created    int `json:"created"`
	Linked     int `json:"linked"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Synced     int `json:"synced"`
	SyncFailed int `json:"syncFailed"`
}

func (h *Handler) convertWebToBuild(c *gin.Context) {
	var request buildConversionRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "转换请求无效")
		return
	}
	if request.All && len(request.IDs) > 0 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "全部转换与指定账号不能同时提交")
		return
	}
	if request.Strategy == "" {
		request.Strategy = accountapp.BuildConversionMissing
	}
	if request.Strategy != accountapp.BuildConversionAll && request.Strategy != accountapp.BuildConversionMissing {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "转换策略无效")
		return
	}
	var ids []uint64
	if !request.All {
		var err error
		ids, err = parseIDs(request.IDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
			return
		}
		if !h.validateProviderIDs(c, ids, string(accountdomain.ProviderWeb)) {
			return
		}
	}
	h.streamWebToBuildConversion(c, request.All, ids, request.Strategy)
}

func (h *Handler) syncWebToConsole(c *gin.Context) {
	var request webConsoleSyncRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "同步请求无效")
		return
	}
	if request.All && len(request.IDs) > 0 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "全部同步与指定账号不能同时提交")
		return
	}
	if request.Strategy == "" {
		request.Strategy = accountapp.WebConsoleSyncAll
	}
	if request.Strategy != accountapp.WebConsoleSyncAll && request.Strategy != accountapp.WebConsoleSyncMissing {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "同步策略无效")
		return
	}
	var ids []uint64
	if !request.All {
		var err error
		ids, err = parseIDs(request.IDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
			return
		}
		if !h.validateProviderIDs(c, ids, string(accountdomain.ProviderWeb)) {
			return
		}
	}
	h.streamWebToConsoleSync(c, request.All, ids, request.Strategy)
}

func (h *Handler) streamWebToConsoleSync(c *gin.Context, all bool, ids []uint64, strategy accountapp.WebConsoleSyncStrategy) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	var total atomic.Int64
	result, syncResult, err := h.onboarding.SyncToConsole(c.Request.Context(), all, ids, strategy, stream.PhaseProgressObserver("importing", &total), stream.SyncProgressObserver())
	if err != nil {
		stream.WriteError("accountConsoleSyncFailed", "Grok Web 账号同步到 Console 失败")
		return
	}
	_ = stream.Write("complete", newAccountImportResponse(result, syncResult))
}

func (h *Handler) streamWebToBuildConversion(c *gin.Context, all bool, ids []uint64, strategy accountapp.BuildConversionStrategy) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	var total atomic.Int64
	result, syncResult, err := h.onboarding.ConvertToBuild(c.Request.Context(), all, ids, strategy, stream.PhaseProgressObserver("converting", &total), stream.SyncProgressObserver())
	if err != nil {
		stream.WriteError("accountConversionFailed", "Grok Web 账号转换失败")
		return
	}
	_ = stream.Write("complete", newBuildConversionResponse(result, syncResult))
}

func newBuildConversionResponse(result accountapp.BuildConversionResult, syncResult accountsyncapp.Result) buildConversionResponse {
	return buildConversionResponse{
		Created: result.Created, Linked: result.Linked, Skipped: result.Skipped, Failed: result.Failed,
		Synced: syncResult.Succeeded, SyncFailed: syncResult.Failed,
	}
}
