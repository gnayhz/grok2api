package httphelpers

import (
	"errors"
	"strconv"
)

// InvalidIDError 报告 ID 列表中首个无法解析的文本。Error() 沿用 model /
// clientkey 面历史文案「无效 ID: <value>」；需要其他文案的调用方用
// InvalidIDValue 取回原文自行组装，或在外层适配为固定消息（account 面为
// 历史兼容保留固定文案「ID 无效」，不携带具体值）。
type InvalidIDError struct {
	Value string
}

func (e *InvalidIDError) Error() string {
	return "无效 ID: " + e.Value
}

// ParseIDs 逐个解析正整数 ID：非数字、空串或 0 均非法（历史三份实现口径：
// ParseUint 失败或结果为 0）。任一值非法时立即返回 *InvalidIDError。
// 空列表返回空切片且不报错。
func ParseIDs(values []string) ([]uint64, error) {
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, &InvalidIDError{Value: value}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// InvalidIDValue 返回 ParseIDs 报错时首个非法 ID 的原始文本；err 不是
// *InvalidIDError 时返回空串。
func InvalidIDValue(err error) string {
	var invalid *InvalidIDError
	if errors.As(err, &invalid) {
		return invalid.Value
	}
	return ""
}
