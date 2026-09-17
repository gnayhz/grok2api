// Package selector owns M06 candidate planning, lease acquisition and CAS revalidation.
//
// export.go is the production forwarding surface gateway consumes (attempt
// resources, admission bodies, selection sessions, probe candidates) plus a
// few other-package test seams (StickySessionKey, ReplaceAccountStore,
// LocalQualityAllowed). Those files must stay one-line forwards.
package selector
