package gateway

import (
	"context"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

func TestApplyMediaJobEgressFields(t *testing.T) {
	t.Parallel()
	_, trace := infraegress.WithTrace(context.Background())
	trace.Record(infraegress.Selection{NodeID: 7, NodeName: "media-node", Scope: egress.ScopeBuild, Proxied: true})
	job := &media.Job{}
	applyMediaJobEgress(job, trace, accountdomain.ProviderBuild)
	if job.EgressNodeName != "media-node" || job.EgressMode != string(audit.EgressModeProxy) || job.EgressScope != string(egress.ScopeBuild) {
		t.Fatalf("job = %+v", job)
	}
	if job.EgressNodeID == nil || *job.EgressNodeID != 7 {
		t.Fatal("node id must carry onto the media job")
	}
}
