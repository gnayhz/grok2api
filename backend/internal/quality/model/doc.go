// Package model 是质量层的领域词汇表:Party/Case/Verdict/状态枚举。
// 纯类型零依赖(B4 依赖铁律 1):只依赖标准库,不 import 任何工程包。
// 状态机合法性规则(T1-T9 等)以数据登记,由 registry 在写入时强制。
package model
