package relational

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 锁定读取兼容：存量库中存在 API 合同形状（字符串数字）的 routing 载荷。
// 容错解析失败时 toEgressOperationsConfigDomain 会整体退回自动调度，
// 运营配置形同虚设（生产日志每分钟 WARN 刷屏）。三种形态都必须还原成
// 同一领域值；写回路径保持规范数字形态不变。
func TestEgressRoutingPayloadToleratesLegacyStringShape(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-routing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewEgressRepository(database)

	canonical := `{"default":{"mode":"node","nodeId":52}}`
	legacy := `{"default":{"mode":"node","nodeId":"52"}}`

	for name, payload := range map[string]string{"canonical": canonical, "legacy-string": legacy} {
		t.Run(name, func(t *testing.T) {
			row := egressOperationsConfigModel{ID: 1, ProbeProvider: "cloudflare", ProbeIntervalSeconds: 60, Routing: payload, UpdatedAt: time.Now().UTC()}
			if err := database.db.WithContext(ctx).Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			config, err := repository.GetEgressOperationsConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !config.DefaultTarget.Configured() || config.DefaultTarget.Mode != egressdomain.RoutingTargetNode || config.DefaultTarget.NodeID != 52 {
				t.Fatalf("default target = %#v, want node/52", config.DefaultTarget)
			}
			if err := database.db.WithContext(ctx).Delete(&egressOperationsConfigModel{}, row.ID).Error; err != nil {
				t.Fatal(err)
			}
		})
	}

	// 规范写出：结构体编码永远产生数字形态。
	encoded, err := json.Marshal(egressRoutingPayload{Default: &egressRoutingTargetRow{Mode: "node", NodeID: 52}})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != canonical {
		t.Fatalf("marshal = %s, want %s", encoded, canonical)
	}

	// 垃圾输入仍按既有语义失败 → 调用方退化自动调度，而非误判配置。
	var corrupt egressRoutingTargetRow
	if err := json.Unmarshal([]byte(`{"mode":"node","nodeId":"not-a-number"}`), &corrupt); err == nil {
		t.Fatal("corrupt nodeId accepted")
	}
}
