package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/app"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/observability"
)

func runAttributeRisk(args []string) error {
	configPath := defaultConfigPath()
	idsFile := ""
	timeout := 15 * time.Minute
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--config":
			if index+1 >= len(args) {
				return fmt.Errorf("attribute-risk --config 缺少路径")
			}
			configPath = args[index+1]
			index++
		case "--ids-file":
			if index+1 >= len(args) {
				return fmt.Errorf("attribute-risk --ids-file 缺少路径")
			}
			idsFile = args[index+1]
			index++
		case "--timeout":
			if index+1 >= len(args) {
				return fmt.Errorf("attribute-risk --timeout 缺少时长")
			}
			parsed, err := time.ParseDuration(args[index+1])
			if err != nil {
				return fmt.Errorf("attribute-risk --timeout: %w", err)
			}
			timeout = parsed
			index++
		default:
			return fmt.Errorf("attribute-risk 不支持的参数: %s", args[index])
		}
	}
	if idsFile == "" {
		return fmt.Errorf("attribute-risk 需要 --ids-file")
	}
	ids, err := readIDFile(idsFile)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("attribute-risk ids-file 为空")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logger := observability.NewLogger()
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	application, err := app.New(bootCtx, cfg, logger)
	bootCancel()
	if err != nil {
		return err
	}
	defer func() { _ = application.Close() }()
	attrCtx, attrCancel := context.WithTimeout(context.Background(), timeout)
	defer attrCancel()
	logger.Info("attribute_risk_start", "ids", len(ids), "timeout", timeout.String())
	if err := application.AttributeRiskAccounts(attrCtx, ids); err != nil {
		return err
	}
	logger.Info("attribute_risk_done", "ids", len(ids), "ctx_err", fmt.Sprint(attrCtx.Err()))
	return nil
}
