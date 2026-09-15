package model

import "strings"

// ExitIdentity 是出口的双地址族身份。任一非空族不同即视为出口身份
// 变化——与 egress 轮换验证(exitIPRotationChanged)同一把尺子:部分
// 隧道(MicroWARP/WARP 类)重启后 IPv4 稳定而 IPv6 每次轮换,只比较
// 单一聚合 IP 会把这类节点的真实换 IP 判成"未变化"。
//
// 空族表示"未知/未观测",不参与差异比较;升级前只保存聚合 IP 的旧档
// 案首次观测到另一族时,由登记处采纳补写基线而不翻 epoch(见 registry
// 的采纳规则),避免升级本身释放仍有效的限制。
type ExitIdentity struct {
	IPv4 string
	IPv6 string
}

// Present 报告是否至少观测到一个地址族。两族皆空不是可用的观测。
func (i ExitIdentity) Present() bool {
	return i.IPv4 != "" || i.IPv6 != ""
}

// Aggregate 返回与探活聚合口径一致的单串展示身份(IPv4 优先,缺失时
// 用 IPv6)。档案行 current_ip 与降智台账继续使用该口径。
func (i ExitIdentity) Aggregate() string {
	if i.IPv4 != "" {
		return i.IPv4
	}
	return i.IPv6
}

// ExitIdentityFromAggregate 把历史单串 IP 按地址形态归入地址族
// (含":"视为 IPv6)。仅供旧调用面/旧档案迁移使用;新观测应直接
// 构造双族身份。
func ExitIdentityFromAggregate(ip string) ExitIdentity {
	if ip == "" {
		return ExitIdentity{}
	}
	if strings.Contains(ip, ":") {
		return ExitIdentity{IPv6: ip}
	}
	return ExitIdentity{IPv4: ip}
}
