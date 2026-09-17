// Package crypto 保存内圈安全合同：凭据加解密面、口令哈希与校验、管理员
// access token 签发/校验、随机 token 能力及其稳定值。AES、JWT、bcrypt 与
// 随机源实现在 infra/security，由组合根注入；客户端 Key 的 g2a 格式规则
// 归 domain/clientkey；确定性摘要在 pkg/tokenhash。
package crypto
