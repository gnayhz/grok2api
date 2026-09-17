// Package port 保存内圈端口：application/domain 可依赖的合同，而不是 SQL、HTTP 或上游协议实现。
//
// 端口按能力分目录（provider、physical、crypto、lifecycle、updatecheck）；
// infra 实现这些端口，组合根负责装配。依赖方向由此固定：infra 不 import
// application，应用层只消费端口类型。
package port
