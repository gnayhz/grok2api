package qualityhttp

import (
	"encoding/json"
	"strconv"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
)

// Resource IDs cross the browser boundary as decimal strings. Numeric request
// IDs remain accepted for existing callers without passing through float64.
type resourceID uint64

func (id resourceID) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(id), 10))
}
func (id *resourceID) UnmarshalJSON(data []byte) error {
	value := string(data)
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err == nil {
		*id = resourceID(n)
	}
	return err
}

type resourceCheckDTO struct {
	management.ResourceCheck
	ID         resourceID              `json:"id"`
	ResourceID resourceID              `json:"resource_id"`
	Report     *resourceCheckReportDTO `json:"report,omitempty"`
}
type resourceCheckReportDTO struct {
	management.ResourceCheckReport
	ResourceID resourceID              `json:"resource_id"`
	Groups     []resourceCheckGroupDTO `json:"groups"`
}
type resourceCheckGroupDTO struct {
	management.ResourceCheckGroup
	ControlAccount resourceID               `json:"control_account"`
	ControlNode    resourceID               `json:"control_node"`
	AccountID      resourceID               `json:"account_id"`
	NodeID         resourceID               `json:"node_id"`
	IdentityGroup  resourceID               `json:"identity_group"`
	Control        []resourceCheckSampleDTO `json:"control"`
	Samples        []resourceCheckSampleDTO `json:"samples"`
	After          *resourceCheckSampleDTO  `json:"after,omitempty"`
}
type resourceCheckSampleDTO struct {
	management.AccountCheckSample
	PathBinding resourceID              `json:"path_binding,omitempty"`
	Attempt     resourceCheckAttemptDTO `json:"attempt"`
}
type resourceCheckAttemptDTO struct {
	attemptmeta.Identity
	AccountID resourceID           `json:"account_id"`
	Revision  resourceID           `json:"revision"`
	Path      resourceCheckPathDTO `json:"path"`
}
type resourceCheckPathDTO struct {
	attemptmeta.Path
	NodeID resourceID `json:"node_id,omitempty"`
	Epoch  resourceID `json:"epoch,omitempty"`
}

func checkSampleDTO(s management.AccountCheckSample) resourceCheckSampleDTO {
	return resourceCheckSampleDTO{AccountCheckSample: s, PathBinding: resourceID(s.PathBinding), Attempt: resourceCheckAttemptDTO{
		Identity: s.Attempt, AccountID: resourceID(s.Attempt.AccountID), Revision: resourceID(s.Attempt.Revision),
		Path: resourceCheckPathDTO{Path: s.Attempt.Path, NodeID: resourceID(s.Attempt.Path.NodeID), Epoch: resourceID(s.Attempt.Path.Epoch)},
	}}
}
func checkSamplesDTO(samples []management.AccountCheckSample) []resourceCheckSampleDTO {
	items := make([]resourceCheckSampleDTO, 0, len(samples))
	for _, sample := range samples {
		items = append(items, checkSampleDTO(sample))
	}
	return items
}
func checkDTO(check management.ResourceCheck) resourceCheckDTO {
	result := resourceCheckDTO{ResourceCheck: check, ID: resourceID(check.ID), ResourceID: resourceID(check.ResourceID)}
	if check.Report == nil {
		return result
	}
	result.Report = &resourceCheckReportDTO{ResourceCheckReport: *check.Report, ResourceID: resourceID(check.Report.ResourceID), Groups: []resourceCheckGroupDTO{}}
	for _, group := range check.Report.Groups {
		g := resourceCheckGroupDTO{ResourceCheckGroup: group, ControlAccount: resourceID(group.ControlAccount), ControlNode: resourceID(group.ControlNode), AccountID: resourceID(group.AccountID), NodeID: resourceID(group.NodeID), IdentityGroup: resourceID(group.IdentityGroup), Control: checkSamplesDTO(group.Control), Samples: checkSamplesDTO(group.Samples)}
		if group.After != nil {
			s := checkSampleDTO(*group.After)
			g.After = &s
		}
		result.Report.Groups = append(result.Report.Groups, g)
	}
	return result
}
