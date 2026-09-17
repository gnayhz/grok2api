package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	portcrypto "github.com/chenyme/grok2api/backend/internal/port/crypto"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// QualityQuarantiner is the infra-level transport-cooldown surface
// implemented by the egress Manager. 旧质量隔离/软冷却状态已随旧质量链
// 删除(切换手册第2步);本接口现在只承载死出口确认(probe_dead)的
// 传输冷却——传输健康轴与质量轴独立,前者归底座。
type QualityQuarantiner interface {
	// CooldownNodeForProbeFailure applies a transport cooldown after a
	// confirmed dead exit (both address families failed twice in a row).
	// Recovery is automatic: the next healthy probe clears it.
	CooldownNodeForProbeFailure(ctx context.Context, nodeID uint64, until time.Time) error
}

// SetQualityQuarantiner installs the infra transport-cooldown adapter.
func (s *Service) SetQualityQuarantiner(value QualityQuarantiner) {
	if s == nil || value == nil {
		return
	}
	s.mu.Lock()
	s.qualityQuarantiner = value
	s.mu.Unlock()
}

// SetQualityLogger installs a dedicated logger for the egress guard surface.
func (s *Service) SetQualityLogger(value *slog.Logger) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if value != nil {
		s.qualityLogger = value
	}
	s.mu.Unlock()
}

func (s *Service) qualityLog() *slog.Logger {
	if s == nil {
		return slog.Default()
	}
	s.mu.RLock()
	logger := s.qualityLogger
	s.mu.RUnlock()
	if logger == nil {
		return slog.Default()
	}
	return logger
}

// BatchRotationResult reports a batch template application.
type BatchRotationResult struct {
	Updated int
	Skipped int
}

// BatchSetNodeRotation applies one rotation-webhook template to many nodes at
// once. Supported placeholders: {name} (node name, URL-escaped), {host} and
// {port} (from the node's decrypted proxy URL). An empty template clears the
// webhook for the selected nodes. Nodes whose proxy URL cannot be resolved
// (e.g. no port while the template references {port}) are skipped, not failed.
func (s *Service) BatchSetNodeRotation(ctx context.Context, ids []uint64, template string) (BatchRotationResult, error) {
	result := BatchRotationResult{}
	if s == nil || s.repository == nil {
		return result, ErrOperationsUnavailable
	}
	if len(ids) == 0 || len(ids) > 5000 {
		return result, fmt.Errorf("%w: 节点数量必须在 1 到 5000 之间", ErrInvalidInput)
	}
	template = strings.TrimSpace(template)
	if template != "" {
		// 占位符先替换成样本再校验：{host}/{name} 中的花括号会被 url.Parse 拒绝。
		sample := strings.NewReplacer("{name}", "sample", "{host}", "example.invalid", "{port}", "1").Replace(template)
		value, err := url.Parse(sample)
		if err != nil || (value.Scheme != "http" && value.Scheme != "https") || value.Host == "" {
			return result, fmt.Errorf("%w: 换 IP webhook 模板必须是 http(s) URL", ErrInvalidInput)
		}
		if len(template) > maxRotationURLBytes {
			return result, fmt.Errorf("%w: 换 IP webhook 模板过长", ErrInvalidInput)
		}
	}
	repo, ok := s.repository.(rotationURLWriter)
	if !ok {
		return result, ErrOperationsUnavailable
	}
	for _, id := range ids {
		node, err := s.repository.GetEgressNode(ctx, id)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				result.Skipped++
				continue
			}
			return result, err
		}
		if template == "" {
			// 清空对一切节点照常执行:对误配过 webhook 的代理池节点是修复动作,
			// 把它带回互斥校验认可的合法状态。
			if err := repo.UpdateEgressNodeRotationURL(ctx, id, "", false); err != nil {
				return result, err
			}
			result.Updated++
			continue
		}
		// 互斥:批量套用 webhook 模板跳过代理池节点——外部代理池的出口由服务
		// 商自动更换,webhook 无意义,写入会产生单节点编辑保存被互斥校验拒绝的
		// 非法状态(历史缺陷:批量模板曾绕过 applyInput 的互斥校验)。
		if node.ProxyPool {
			result.Skipped++
			continue
		}
		resolved, resolvable := resolveRotationTemplate(template, node, s.cipher)
		if !resolvable {
			result.Skipped++
			continue
		}
		encrypted, err := s.cipher.Encrypt(resolved)
		if err != nil {
			return result, err
		}
		if err := repo.UpdateEgressNodeRotationURL(ctx, id, encrypted, true); err != nil {
			return result, err
		}
		result.Updated++
	}
	return result, nil
}

type rotationURLWriter interface {
	// UpdateEgressNodeRotationURL 同步落库 webhook 密文与启用开关:写入非空
	// webhook 即启用轮换, 清空即关闭——与单节点编辑语义、以及 schema 一次性
	// 回填(enabled 跟随 encrypted_rotation_url)的语义保持一致, 避免按部署
	// 指南批量设置后自动换 IP 闭环静默失效。
	UpdateEgressNodeRotationURL(ctx context.Context, id uint64, encryptedRotationURL string, enabled bool) error
}

// resolveRotationTemplate substitutes {name}/{host}/{port} for one node. The
// second return is false when the template needs a port the proxy URL lacks.
func resolveRotationTemplate(template string, node domain.Node, cipher portcrypto.Cryptor) (string, bool) {
	proxyURL := ""
	if cipher != nil && strings.TrimSpace(node.EncryptedProxyURL) != "" {
		if decrypted, err := cipher.Decrypt(node.EncryptedProxyURL); err == nil {
			proxyURL = decrypted
		}
	}
	var host, port string
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil && parsed.Host != "" {
			host, port = "", ""
			if splitHost, splitPort, splitErr := net.SplitHostPort(parsed.Host); splitErr == nil {
				host, port = splitHost, splitPort
			} else {
				host = parsed.Hostname()
			}
			if port == "" {
				port = parsed.Port()
			}
		}
	}
	if strings.Contains(template, "{port}") && port == "" {
		return "", false
	}
	resolved := strings.ReplaceAll(template, "{name}", url.PathEscape(node.Name))
	resolved = strings.ReplaceAll(resolved, "{host}", host)
	resolved = strings.ReplaceAll(resolved, "{port}", port)
	return resolved, true
}
