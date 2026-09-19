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
	Revision     resourceID               `json:"revision"`
	ResourceID   resourceID               `json:"resource_id"`
	Observations []resourceObservationDTO `json:"observations"`
	Results      []resourceProofDTO       `json:"results"`
}
type resourceObservationDTO struct {
	management.ResourceObservation
	AccountID     resourceID             `json:"account_id"`
	NodeID        resourceID             `json:"node_id"`
	IdentityGroup resourceID             `json:"identity_group"`
	Sample        resourceCheckSampleDTO `json:"sample"`
}
type resourceProofDTO struct {
	management.ResourceProof
	ResourceID    resourceID `json:"resource_id"`
	IdentityGroup resourceID `json:"identity_group"`
}
type resourceCheckSampleDTO struct {
	management.ResourceSample
	CredentialGeneration resourceID              `json:"credential_generation"`
	PathBinding          resourceID              `json:"path_binding,omitempty"`
	Attempt              resourceCheckAttemptDTO `json:"attempt"`
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

func checkSampleDTO(s management.ResourceSample) resourceCheckSampleDTO {
	return resourceCheckSampleDTO{ResourceSample: s, CredentialGeneration: resourceID(s.CredentialGeneration), PathBinding: resourceID(s.PathBinding), Attempt: resourceCheckAttemptDTO{
		Identity: s.Attempt, AccountID: resourceID(s.Attempt.AccountID), Revision: resourceID(s.Attempt.Revision),
		Path: resourceCheckPathDTO{Path: s.Attempt.Path, NodeID: resourceID(s.Attempt.Path.NodeID), Epoch: resourceID(s.Attempt.Path.Epoch)},
	}}
}
func checkDTO(check management.ResourceCheck) resourceCheckDTO {
	result := resourceCheckDTO{ResourceCheck: check, ID: resourceID(check.ID), ResourceID: resourceID(check.ResourceID)}
	if check.Report == nil {
		return result
	}
	result.Report = &resourceCheckReportDTO{ResourceCheckReport: *check.Report, Revision: resourceID(check.Report.Revision), ResourceID: resourceID(check.Report.ResourceID), Observations: []resourceObservationDTO{}, Results: []resourceProofDTO{}}
	for _, o := range check.Report.Observations {
		result.Report.Observations = append(result.Report.Observations, resourceObservationDTO{ResourceObservation: o, AccountID: resourceID(o.AccountID), NodeID: resourceID(o.NodeID), IdentityGroup: resourceID(o.IdentityGroup), Sample: checkSampleDTO(o.Sample)})
	}
	for _, p := range check.Report.Results {
		if p.Evidence == nil {
			p.Evidence = []int{}
		}
		result.Report.Results = append(result.Report.Results, resourceProofDTO{ResourceProof: p, ResourceID: resourceID(p.ResourceID), IdentityGroup: resourceID(p.IdentityGroup)})
	}
	return result
}
