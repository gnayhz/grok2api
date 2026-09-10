package proxy

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// TestNodeTypeClassification 锚定节点三型学(基准 2.1)。
func TestNodeTypeClassification(t *testing.T) {
	cases := []struct {
		profile NodeProfile
		want    model.NodeType
	}{
		{NodeProfile{ID: 1}, model.NodeFixed},
		{NodeProfile{ID: 2, RotationWebhook: true}, model.NodeWebhook},
		{NodeProfile{ID: 3, ProxyPool: true, PoolSticky: true}, model.NodePoolSticky},
		{NodeProfile{ID: 4, ProxyPool: true, PoolSticky: false}, model.NodePoolPerRequest},
	}
	for _, c := range cases {
		if got := c.profile.Type(); got != c.want {
			t.Fatalf("节点 %d 型 = %s, want %s", c.profile.ID, got, c.want)
		}
	}
}

// TestSelectionDistributionObservation 锚定 G10 观测面:拨号选择分布计数。
func TestSelectionDistributionObservation(t *testing.T) {
	policy := NewDialerPolicy()
	policy.ObserveAcquisition("grok_build", 1)
	policy.ObserveAcquisition("grok_build", 1)
	policy.ObserveAcquisition("grok_build", 2)
	distribution := policy.SelectionDistribution()
	if distribution["grok_build"][1] != 2 || distribution["grok_build"][2] != 1 {
		t.Fatalf("分布 = %v", distribution)
	}
}
