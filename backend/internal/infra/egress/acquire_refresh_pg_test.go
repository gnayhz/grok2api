package egress

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	relational "github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// 真实 PostgreSQL 上的出口获取延迟分布:稳态(快照缓存命中)对照
// TTL 过期首请求(完整 DB 回源: 100 行 SELECT + AES-GCM 解密重建
// poolFlags + 快照安装)。环境变量 TEST_POSTGRES_DSN 未配置时跳过;
// 复现命令见 scripts/verify-postgres-migrations.sh 同款容器。
func TestAcquireRefreshLatencyDistributionOnPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := relational.OpenPostgres(ctx, dsn, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewEgressRepository(database)
	prefix := fmt.Sprintf("refresh-bench-%d", time.Now().UTC().UnixNano())
	for i := 0; i < 100; i++ {
		proxy, encryptErr := cipher.Encrypt(fmt.Sprintf("http://10.%d.%d.1:8080", i/250, i%250))
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		if _, err := repo.CreateEgressNode(ctx, domain.Node{Name: fmt.Sprintf("%s-%03d", prefix, i), Enabled: true, EncryptedProxyURL: proxy, Health: 1}); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewManager(repo, cipher)

	sample := func(refresh bool) []time.Duration {
		const samples = 200
		latencies := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			if refresh {
				manager.nodeMu.Lock()
				delete(manager.nodes, nodeSnapshotKey)
				manager.nodeMu.Unlock()
			}
			started := time.Now()
			lease, acquireErr := manager.Acquire(ctx, domain.ScopeBuild, fmt.Sprintf("acct-%d", i%64))
			latencies = append(latencies, time.Since(started))
			if acquireErr != nil {
				t.Fatalf("acquire: %v", acquireErr)
			}
			lease.Release()
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		return latencies
	}

	percentile := func(latencies []time.Duration, p float64) time.Duration {
		return latencies[int(float64(len(latencies)-1)*p)]
	}
	steady := sample(false)
	refresh := sample(true)
	t.Logf("steady-state  p50=%v p99=%v max=%v", percentile(steady, 0.50), percentile(steady, 0.99), steady[len(steady)-1])
	t.Logf("ttl-expiry-1st p50=%v p99=%v max=%v", percentile(refresh, 0.50), percentile(refresh, 0.99), refresh[len(refresh)-1])
	// 回源路径必须仍处于交互级延迟:1s p99 上限只是防回归护栏(真实值应低
	// 两个数量级);超限说明回源路径引入了阻塞点(同步 DB 写/锁竞争)。
	if guard := percentile(refresh, 0.99); guard > time.Second {
		t.Fatalf("ttl-expiry acquire p99 = %v, regression guard is 1s", guard)
	}
}
