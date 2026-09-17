package provider

import inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"

// ThinkingEvidenceComment remains the converter/scanner contract.
// The canonical value lives in domain/inference so HTTP need not import this port for the marker.
const ThinkingEvidenceComment = inferencedomain.ThinkingEvidenceComment
