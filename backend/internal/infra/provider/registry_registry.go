package provider

// Registry implementation, registration half: constructor, registration,
// Get/Definition/Providers and assembly validation. The concrete registry
// lives at the dialect boundary ; the port keeps the interface.

import (
	"errors"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	portprovider "github.com/chenyme/grok2api/backend/internal/port/provider"
)

type Registry struct {
	adapters    map[account.Provider]portprovider.Adapter
	definitions map[account.Provider]portprovider.Definition
	issues      []error
}

func NewRegistry(adapters ...portprovider.Adapter) *Registry {
	registry := &Registry{
		adapters:    make(map[account.Provider]portprovider.Adapter, len(adapters)),
		definitions: make(map[account.Provider]portprovider.Definition, len(adapters)),
	}
	for _, adapter := range adapters {
		if adapter == nil {
			registry.issues = append(registry.issues, errors.New("Provider Adapter 不能为空"))
			continue
		}
		providerValue := adapter.Provider()
		if !providerValue.IsValid() {
			registry.issues = append(registry.issues, fmt.Errorf("Provider Adapter 身份 %q 无效", providerValue))
			continue
		}
		if _, exists := registry.adapters[providerValue]; exists {
			registry.issues = append(registry.issues, fmt.Errorf("Provider %s 重复注册", providerValue))
			continue
		}
		registry.adapters[providerValue] = adapter
		if source, ok := adapter.(portprovider.DefinitionAdapter); ok {
			registry.definitions[providerValue] = source.Definition().Clone()
		}
	}
	return registry
}

// Get returns a registered Provider Adapter.
func (r *Registry) Get(value account.Provider) (portprovider.Adapter, bool) {
	adapter, ok := r.adapters[value]
	return adapter, ok
}

// Definition returns the stable capability declaration from a production Adapter.
func (r *Registry) Definition(value account.Provider) (portprovider.Definition, bool) {
	definition, ok := r.definitions[value]
	return definition.Clone(), ok
}

// Providers returns registered Providers in fixed channel order with capability definitions.
func (r *Registry) Providers() []account.Provider {
	values := make([]account.Provider, 0, len(r.definitions))
	for _, value := range account.Providers() {
		if _, ok := r.definitions[value]; ok {
			values = append(values, value)
		}
	}
	return values
}

// Validate checks that production registry definitions match their implemented capability interfaces.
func (r *Registry) Validate() error {
	if r == nil {
		return errors.New("Provider Registry 不能为空")
	}
	if len(r.issues) > 0 {
		return errors.Join(r.issues...)
	}
	for _, value := range account.Providers() {
		adapter, registered := r.adapters[value]
		definition, described := r.definitions[value]
		if !registered || !described {
			return fmt.Errorf("Provider %s 未完整注册 Adapter 与 Definition", value)
		}
		if definition.Provider != value {
			return fmt.Errorf("Provider %s 的 Definition 身份不一致", value)
		}
		if err := definition.Validate(); err != nil {
			return err
		}
		if definition.Conversation.Responses || definition.Conversation.ChatCompletions || definition.Conversation.Messages {
			if _, ok := adapter.(portprovider.ResponseAdapter); !ok {
				return fmt.Errorf("Provider %s 声明对话能力但未实现适配器", value)
			}
		}
		if _, ok := adapter.(portprovider.ModelCatalogAdapter); !ok {
			return fmt.Errorf("Provider %s 未实现模型目录适配器", value)
		}
		switch definition.Quota {
		case portprovider.QuotaBilling:
			if _, ok := adapter.(portprovider.BillingAdapter); !ok {
				return fmt.Errorf("Provider %s 声明 Billing 额度但未实现适配器", value)
			}
		case portprovider.QuotaRemoteWindow, portprovider.QuotaLocalWindow:
			if _, ok := adapter.(portprovider.QuotaAdapter); !ok {
				return fmt.Errorf("Provider %s 声明窗口额度但未实现适配器", value)
			}
		}
		if definition.Credential.Import {
			if _, ok := adapter.(portprovider.CredentialCodecAdapter); !ok {
				return fmt.Errorf("Provider %s 声明凭据导入但未实现适配器", value)
			}
		}
		if definition.Credential.Refresh {
			if _, ok := adapter.(portprovider.CredentialRefreshAdapter); !ok {
				return fmt.Errorf("Provider %s 声明凭据刷新但未实现适配器", value)
			}
		}
		if definition.Credential.DeviceOAuth {
			if _, ok := adapter.(portprovider.DeviceOAuthAdapter); !ok {
				return fmt.Errorf("Provider %s 声明 Device OAuth 但未实现适配器", value)
			}
		}
		if definition.Media.ImageGeneration {
			if _, ok := adapter.(portprovider.ImageGenerationAdapter); !ok {
				return fmt.Errorf("Provider %s 声明图像生成能力但未实现适配器", value)
			}
		}
		if definition.Media.ImageEdit {
			if _, ok := adapter.(portprovider.ImageEditAdapter); !ok {
				return fmt.Errorf("Provider %s 声明图像编辑能力但未实现适配器", value)
			}
		}
		if definition.Media.VideoGeneration {
			if _, ok := adapter.(portprovider.VideoAdapter); !ok {
				return fmt.Errorf("Provider %s 声明视频能力但未实现适配器", value)
			}
		}
		if definition.Media.TTS {
			if _, ok := adapter.(portprovider.TTSAdapter); !ok {
				return fmt.Errorf("Provider %s 声明语音合成能力但未实现适配器", value)
			}
		}
		if definition.Media.STT {
			if _, ok := adapter.(portprovider.STTAdapter); !ok {
				return fmt.Errorf("Provider %s 声明语音识别能力但未实现适配器", value)
			}
		}
		if definition.Media.Realtime {
			if _, ok := adapter.(portprovider.VoiceWebSocketAdapter); !ok {
				return fmt.Errorf("Provider %s 声明实时语音能力但未实现 WebSocket 适配器", value)
			}
		}
	}
	return nil
}
