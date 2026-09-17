// Package httphelpers 提供各 HTTP handler 共用的解析与响应工具：ID 列表、
// 路径参数与分页解析（httphelpers.go/ids.go），OpenAI 与 Anthropic 错误信封
// （errors.go），SSE 事件帧与流式写截止时间（sse.go）。
// 此前 model/account/clientkey/egress/audit/media 各自维护逐字节相同的实现；
// 统一到一处后各调用方的状态码、错误码与文案保持不变，差异只在调用方通过
// 参数保留（例如各面的 invalidId 文案与游标面缺省页大小）。
package httphelpers

import (
	"net/http"
	"strconv"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

// Pagination 读取 page/pageSize 查询参数并按仓储分页规则归一化；
// 缺省 page=1、pageSize=repository.DefaultPageSize，非法值由
// repository.NormalizePage 统一收敛（下限 1、上限 MaxPageSize）。
func Pagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", strconv.Itoa(repository.DefaultPageSize)))
	return repository.NormalizePage(page, size, repository.DefaultPageSize)
}

// CursorPagination 读取游标分页的 pageSize 并按仓储分页规则归一化；
// 缺省 repository.DefaultCursorPageSize（游标面无页码）。
func CursorPagination(c *gin.Context) int {
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", strconv.Itoa(repository.DefaultCursorPageSize)))
	_, normalized := repository.NormalizePage(1, size, repository.DefaultCursorPageSize)
	return normalized
}

// PathID 解析路径参数 :id；缺失或非法时写出 invalidId 错误并返回 false。
func PathID(c *gin.Context) (uint64, bool) {
	return PathParamID(c, "id", "ID 无效")
}

// PathParamID 解析指定名称的路径参数；缺失、非数字或为 0 时以 invalidId
// 错误码写出调用方消息并返回 false。消息由调用方给出，以保留各面历史文案。
func PathParamID(c *gin.Context, name, message string) (uint64, bool) {
	id, ok := PathUint(c, name)
	if !ok {
		response.Error(c, http.StatusBadRequest, "invalidId", message)
		return 0, false
	}
	return id, true
}

// PathUint 只解析路径参数为无符号非零 ID，不写任何响应；需要把解析结果
// 与其他校验合并成同一错误（例如同时要求请求体合法）的调用方使用它。
func PathUint(c *gin.Context, name string) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}
