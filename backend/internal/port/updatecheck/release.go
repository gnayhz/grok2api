// Package updatecheck 保存版本查询的内圈端口：来源无关的发布事实与
// 来源合同。版本比较、快照与合并检查归 application/updatecheck；GitHub
// URL、请求头、响应限额与 body 关闭归 infra/updatecheck。
//
// 端口独立成包是为了保持依赖方向：infra 实现合同，application 消费合同，
// 两者不互相 import。
package updatecheck

import "context"

// Release 是来源无关的发布描述。
type Release struct {
	Tag   string
	URL   string
	Notes string
}

// ReleaseSource 提供发布事实；版本排序与快照由应用层拥有。
type ReleaseSource interface {
	LatestRelease(context.Context) (Release, error)
}
