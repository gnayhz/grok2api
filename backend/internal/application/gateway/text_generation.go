package gateway

import (
	"context"
	"math"
	"sort"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
)

// textGeneration is owned by request orchestration until handoff, then by the
// delivery session's once-only finalizer. It retains no payload or lease. The
// physical ledger is authoritative for canonical counters, including attempts
// consumed internally by an adapter and receipts already acknowledged.
type textGeneration struct {
	ctx             context.Context
	entries         map[string]*textGenerationAttempt
	accounts        map[uint64]textGenerationAccount
	selected        string
	source          audit.UsageSource
	pricingModel    string
	currentAccount  uint64
	ordinalFloor    uint64
	responseAbsent  bool
	absentOwnership bool
}

type textGenerationAccount struct {
	name            string
	provider        accountdomain.Provider
	quotaMode       string
	snapshotVersion uint64
}

type textGenerationAttempt struct {
	identity   attemptmeta.Identity
	usage      jsonpeek.TokenUsage
	outcome    string
	responseID string
	status     int
	quotaUnits int
	quotaOwner textGenerationAccount
	receipts   completionReceipt
}

func newTextGeneration(ctx context.Context, source audit.UsageSource, pricingModel string) *textGeneration {
	return &textGeneration{ctx: ctx, entries: make(map[string]*textGenerationAttempt), accounts: make(map[uint64]textGenerationAccount), source: source, pricingModel: pricingModel}
}

func (g *textGeneration) begin(credential accountdomain.Credential, quotaMode string, snapshotVersion uint64) {
	if g == nil {
		return
	}
	g.mergePhysical()
	g.selected = ""
	g.currentAccount, g.responseAbsent, g.absentOwnership = credential.ID, false, false
	for _, fact := range infraegress.PhysicalObservations(g.ctx) {
		g.ordinalFloor = max(g.ordinalFloor, fact.Attempt.Ordinal)
	}
	g.accounts[credential.ID] = textGenerationAccount{name: credential.Name, provider: credential.Provider, quotaMode: quotaMode, snapshotVersion: snapshotVersion}
}

func (g *textGeneration) accept(response *provider.Response, ownership bool) {
	if g == nil {
		return
	}
	g.responseAbsent, g.absentOwnership = response == nil, ownership
	if response == nil || response.Attempt.ID == "" || response.RequestValidation != nil {
		return
	}
	v := &textGenerationAttempt{quotaOwner: g.accounts[response.Attempt.AccountID], identity: response.Attempt, status: response.StatusCode, outcome: "unconfirmed", quotaUnits: max(1, response.QuotaUnits), receipts: completionReceipt{history: "not_required", providerState: "not_required", ownership: "not_required"}}
	if response.CommitOutput != nil {
		v.receipts.history = "not_committed"
	}
	if response.CommitResponseState != nil {
		v.receipts.providerState = "not_committed"
	}
	if ownership {
		v.receipts.ownership = "not_committed"
	}
	g.entries[v.identity.ID] = v
	g.selected = v.identity.ID
}

func (g *textGeneration) withhold() {
	if g != nil {
		g.selected = ""
		g.responseAbsent = false
	}
}

func (g *textGeneration) observeJSON(response *provider.Response, data []byte) {
	if g == nil || response == nil || !jsonpeek.Valid(data) {
		return
	}
	v := g.entries[response.Attempt.ID]
	if v == nil {
		return
	}
	infraegress.ObservePhysicalPayload(g.ctx, v.identity.ID, data)
	// Only a root protocol usage object is eligible; user/tool content is not.
	if raw := jsonpeek.RootRawValue(data, "usage"); len(raw) > 0 {
		if parsed := jsonpeek.TokenUsageObject(raw); parsed.Found {
			v.usage = normalizedGenerationCounters(parsed)
		}
	}
	v.outcome = responsecheck.JSONGeneration(data)
	infraegress.ObservePhysicalGeneration(g.ctx, v.identity.ID, v.outcome)
	v.responseID = jsonpeek.RootStringFieldScan(data, "id")
}

func (g *textGeneration) observeStream(response *provider.Response, usage Usage, completed, failed bool) {
	if g == nil || response == nil {
		return
	}
	v := g.entries[response.Attempt.ID]
	if v == nil {
		return
	}
	if usage.Reported {
		v.usage = normalizedGenerationCounters(physicalUsage(usage))
	}
	if failed {
		v.outcome = "failed"
	} else if completed {
		v.outcome = "completed"
	}
}

func (g *textGeneration) mergePhysical() {
	if g == nil {
		return
	}
	for _, fact := range infraegress.PhysicalObservations(g.ctx) {
		v := g.entries[fact.Attempt.ID]
		if v == nil && (fact.Usage.Found || fact.GenerationOutcome != "") {
			v = &textGenerationAttempt{quotaOwner: g.accounts[fact.Attempt.AccountID], identity: fact.Attempt, status: fact.Status, outcome: "unconfirmed", quotaUnits: 1}
			g.entries[fact.Attempt.ID] = v
		}
		if v != nil && fact.Usage.Found {
			v.usage = normalizedGenerationCounters(fact.Usage)
		}
		if v != nil && fact.GenerationOutcome != "" {
			v.outcome = fact.GenerationOutcome
		}
	}
	// The adapter can finish native generation and then fail before returning
	// a response (for example Web asset archival). In that case the last known
	// generation of this invocation supplies its usage. A replaced or withheld
	// invocation is never selected again by this fallback.
	if g.responseAbsent {
		ordinal := g.ordinalFloor
		for _, v := range g.entries {
			if v.identity.AccountID == g.currentAccount && v.identity.Ordinal > ordinal && (v.usage.Found || v.outcome != "unconfirmed") {
				g.selected, ordinal = v.identity.ID, v.identity.Ordinal
				v.receipts = completionReceipt{history: "not_required", providerState: "not_required", ownership: "not_required"}
				if g.absentOwnership {
					v.receipts.ownership = "not_committed"
				}
			}
		}
	}
}

func (g *textGeneration) finish(response *provider.Response, usage Usage, generation string) (Usage, string) {
	if g == nil {
		return usage, generation
	}
	if response != nil {
		g.observeStream(response, usage, generation == "completed", generation == "failed")
	}
	g.mergePhysical()
	if v := g.entries[g.selected]; v != nil {
		if v.usage.Found {
			usage = usageFromPhysical(v.usage, usage)
		}
		if v.outcome != "unconfirmed" {
			generation = v.outcome
		}
	}
	return usage, generation
}

func usageFromPhysical(v jsonpeek.TokenUsage, prior Usage) Usage {
	return Usage{Reported: v.Found, OutputObserved: prior.OutputObserved, ResponseModel: prior.ResponseModel,
		InputTokens: v.Input, CachedInputTokens: v.Cached, OutputTokens: v.Output, ReasoningTokens: v.Reasoning, TotalTokens: v.Total,
		CostInUSDTicks: v.CostTicks, NumSourcesUsed: v.Sources, NumServerSideToolsUsed: v.ServerTools, ContextInputTokens: v.ContextInput, ContextOutputTokens: v.ContextOutput}
}

func (g *textGeneration) details() []audit.GenerationUsage {
	if g == nil {
		return nil
	}
	g.mergePhysical()
	values := make([]audit.GenerationUsage, 0, len(g.entries))
	for _, entry := range g.entries {
		// An unsuccessful HTTP/transport attempt without generation evidence is
		// already represented by the physical receipt and failure diagnostics.
		if !entry.usage.Found && entry.outcome == "unconfirmed" {
			continue
		}
		v := entry.usage
		value := audit.GenerationUsage{PhysicalID: entry.identity.ID, Ordinal: entry.identity.Ordinal, AccountID: entry.identity.AccountID,
			AccountName: g.accounts[entry.identity.AccountID].name, Model: entry.identity.Model, Selected: entry.identity.ID == g.selected,
			Outcome: entry.outcome, UsageSource: audit.UsageSourceNone, InputTokens: v.Input, CachedInputTokens: v.Cached, CacheCreationTokens: v.CacheCreation,
			OutputTokens: v.Output, ReasoningTokens: v.Reasoning, TotalTokens: v.Total, ContextInputTokens: v.ContextInput, ContextOutputTokens: v.ContextOutput,
			NumSourcesUsed: v.Sources, NumServerSideToolsUsed: v.ServerTools, CostInUSDTicks: v.CostTicks}
		if v.Found {
			value.UsageSource = g.source
			if price, ok := audit.EstimateOfficialCost(g.pricingModel, v.Input, v.Cached, v.Output, v.ContextInput); ok {
				value.EstimatedCostInUSDTicks, value.PricingModel, value.PricingVersion = price.CostInUSDTicks, price.Model, audit.OfficialPricingAsOf
			}
		}
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Ordinal < values[j].Ordinal })
	return values
}

func applyTextUsage(record *audit.Record, usage Usage, source audit.UsageSource, pricingModel string) {
	if usage.Reported {
		record.UsageSource = source
	}
	record.InputTokens, record.CachedInputTokens, record.OutputTokens = usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens
	record.ReasoningTokens, record.TotalTokens, record.CostInUSDTicks = usage.ReasoningTokens, usage.TotalTokens, usage.CostInUSDTicks
	record.NumSourcesUsed, record.NumServerSideToolsUsed = usage.NumSourcesUsed, usage.NumServerSideToolsUsed
	record.ContextInputTokens, record.ContextOutputTokens = usage.ContextInputTokens, usage.ContextOutputTokens
	if usage.Reported {
		if price, ok := audit.EstimateOfficialCost(pricingModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ContextInputTokens); ok {
			record.EstimatedCostInUSDTicks, record.PricingModel, record.PricingVersion = price.CostInUSDTicks, price.Model, audit.OfficialPricingAsOf
		}
	}
}

// finishUnhandedText retains known generation on failure without claiming
// admission, necessary commits or any downstream bytes. Persistence and quota
// updates use the caller's independent, bounded finalization budget.
func (s *Service) finishUnhandedText(record *audit.Record, g *textGeneration, ctx context.Context, guard bool) {
	record.AdmissionOutcome, record.GenerationOutcome, record.DeliveryOutcome = "not_admitted", "unconfirmed", "not_started"
	record.HistoryCommit, record.ProviderStateCommit, record.OwnershipCommit = "not_required", "not_required", "not_required"
	if record.ErrorCode == "request_canceled" {
		record.DeliveryOutcome = "canceled"
	}
	if g != nil {
		usage, generation := g.finish(nil, Usage{}, "unconfirmed")
		record.GenerationOutcome = generation
		applyTextUsage(record, usage, g.source, g.pricingModel)
		record.GenerationUsages = g.details()
		if v := g.entries[g.selected]; v != nil {
			id := v.identity.AccountID
			record.AccountID, record.AccountName = &id, g.accounts[id].name
			record.UpstreamStatusCode, record.ResponseID = v.status, v.responseID
			record.HistoryCommit, record.ProviderStateCommit, record.OwnershipCommit = v.receipts.history, v.receipts.providerState, v.receipts.ownership
		}
		s.finishTextQuotas(newFinalizationBudget(string(record.Operation), record.Provider), g)
	}
	record.PhysicalReceipt = s.finishPhysicalReceipt(ctx)
	record.QualityReceipt = "not_required"
	if guard {
		record.QualityReceipt = "not_recorded"
	}
	if record.ErrorCode == "quality_event_unavailable" {
		record.QualityReceipt = "failed"
	}
}

func (s *Service) finishTextQuotas(budget finalizationBudget, g *textGeneration) {
	if g == nil {
		return
	}
	for _, entry := range g.entries {
		if entry.outcome != "completed" || entry.quotaUnits <= 0 || entry.quotaOwner.quotaMode == "" {
			continue
		}
		owner := entry.quotaOwner
		s.finishQuotaConsumption(budget, accountdomain.QuotaConsumption{EventID: "quota_" + entry.identity.ID, AccountID: entry.identity.AccountID, Mode: owner.quotaMode, SnapshotVersion: owner.snapshotVersion, Units: entry.quotaUnits})
		if kind, _ := s.providers.QuotaKind(owner.provider); kind == provider.QuotaRemoteWindow {
			s.accounts.QueueQuotaRefresh(entry.identity.AccountID, owner.quotaMode)
		}
	}
}

func normalizedGenerationCounters(v jsonpeek.TokenUsage) jsonpeek.TokenUsage {
	v.Input = max(0, v.Input)
	v.Cached = max(0, v.Cached)
	v.CacheCreation = max(0, v.CacheCreation)
	v.Output = max(0, v.Output)
	v.Reasoning = max(0, v.Reasoning)
	v.Total = max(0, v.Total)
	v.ContextInput = max(0, v.ContextInput)
	v.ContextOutput = max(0, v.ContextOutput)
	v.Sources = max(0, v.Sources)
	v.ServerTools = max(0, v.ServerTools)
	v.CostTicks = max(0, v.CostTicks)
	if v.Total == 0 {
		v.Total = math.MaxInt64
		if v.Output <= math.MaxInt64-v.Input {
			v.Total = v.Input + v.Output
		}
	}
	return v
}
