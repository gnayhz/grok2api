// Package model 是质量层的领域词汇表:Party/Case/Verdict/状态枚举。
// 纯规则与词汇表:除标准库外仅依赖 domain/account 与 pkg/attemptmeta 的
// 纯值(身份/代际事实),不 import 任何服务、存储或传输实现。
// 状态机合法性规则(T1-T9 等)以数据登记,由 registry 在写入时强制。
package model
