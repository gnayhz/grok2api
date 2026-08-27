package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/app"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/observability"
)

func runPatrolRisk(args []string) error {
	configPath := defaultConfigPath()
	idsFile := ""
	timeout := 15 * time.Minute
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--config":
			if index+1 >= len(args) {
				return fmt.Errorf("patrol-risk --config 缺少路径")
			}
			configPath = args[index+1]
			index++
		case "--ids-file":
			if index+1 >= len(args) {
				return fmt.Errorf("patrol-risk --ids-file 缺少路径")
			}
			idsFile = args[index+1]
			index++
		case "--timeout":
			if index+1 >= len(args) {
				return fmt.Errorf("patrol-risk --timeout 缺少时长")
			}
			parsed, err := time.ParseDuration(args[index+1])
			if err != nil {
				return fmt.Errorf("patrol-risk --timeout: %w", err)
			}
			timeout = parsed
			index++
		default:
			return fmt.Errorf("patrol-risk 不支持的参数: %s", args[index])
		}
	}
	if idsFile == "" {
		return fmt.Errorf("patrol-risk 需要 --ids-file")
	}
	ids, err := readIDFile(idsFile)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("patrol-risk ids-file 为空")
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
	patrolCtx, patrolCancel := context.WithTimeout(context.Background(), timeout)
	defer patrolCancel()
	logger.Info("patrol_risk_start", "ids", len(ids), "timeout", timeout.String(), "ctx_err", fmt.Sprint(patrolCtx.Err()))
	if err := application.PatrolRiskAccounts(patrolCtx, ids); err != nil {
		return err
	}
	logger.Info("patrol_risk_done", "ids", len(ids), "ctx_err", fmt.Sprint(patrolCtx.Err()))
	return nil
}

func readIDFile(path string) ([]uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read ids-file: %w", err)
	}
	defer file.Close()
	var ids []uint64
	seen := map[uint64]struct{}{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, err := strconv.ParseUint(line, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ids-file 非法 id %q: %w", line, err)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read ids-file: %w", err)
	}
	return ids, nil
}
