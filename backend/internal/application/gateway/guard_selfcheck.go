package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// guardSelfCheckBudget 是自检流的活跃度预算:自检流为内存合成、瞬间读完,
// 预算只防御未来实现回归把自检挂成启动阻塞。
const guardSelfCheckBudget = 2 * time.Second

// GuardSelfCheck 进程内自检:将合成的降智流(reasoning item 闭合、全程零
// 思考增量、usage 谎报非零 reasoning tokens——历史事故中上游降智包的真实
// 形态)与干净流(可见 summary 增量)各过一遍当前扫描器,断言判决分别为
// withhold/deliver。不访问网络;启动时调用一次,防"配置开着、分类器坏了"
// 的组合静默漏放——分类器回归会让自检在部署当时失败,而不是在下次降智
// 攻击时漏放。
func GuardSelfCheck() error {
	cfg := QualityRetryRuntime{Enabled: true, CreatedTimeout: guardSelfCheckBudget, EvidenceTimeout: guardSelfCheckBudget}
	degraded := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_selfcheck"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_selfcheck","summary":[]}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_selfcheck","encrypted_content":"c2VsZmNoZWNr","summary":[]}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_selfcheck"}}`,
		``,
		`data: {"type":"response.output_text.delta","item_id":"msg_selfcheck","delta":"answer"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_selfcheck","output":[],"usage":{"output_tokens":24,"output_tokens_details":{"reasoning_tokens":20}}}}`,
		``,
		``,
	}, "\n")
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(degraded)), qualityProtocolResponses, cfg)
	if replay != nil {
		_ = replay.Close()
	}
	if err != nil {
		return err
	}
	if verdict != QualityWithhold {
		return errors.New("自检降智流未被判扣留: verdict=" + string(verdict))
	}
	clean := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_selfcheck"}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_selfcheck","delta":"thinking evidence"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_selfcheck","output":[]}}`,
		``,
	}, "\n")
	replay, verdict, _, err = peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(clean)), qualityProtocolResponses, cfg)
	if replay != nil {
		_ = replay.Close()
	}
	if err != nil {
		return err
	}
	if verdict != QualityDeliver {
		return errors.New("自检干净流未被判放行: verdict=" + string(verdict))
	}
	return nil
}
