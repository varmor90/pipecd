package client

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/run/v1"
)

// Service and Revision are our own types that wrap the Google API types.
// This allows us to add our own methods on top of them.
type (
	Service  run.Service
	Revision run.Revision
)

const (
	// DefaultServiceManifestFilename is the default name of the Cloud Run service manifest file.
	DefaultServiceManifestFilename = "service.yaml"
)

// Labels used to tag Cloud Run resources managed by PipeCD.
// They allow the plugin to identify which resources it owns.
const (
	LabelManagedBy    = "pipecd-dev-managed-by"    // Always "piped".
	LabelPiped        = "pipecd-dev-piped"         // ID of the piped handling this app.
	LabelApplication  = "pipecd-dev-application"   // ID of the application this resource belongs to.
	LabelCommitHash   = "pipecd-dev-commit-hash"   // Hash of the deployed commit.
	LabelRevisionName = "pipecd-dev-revision-name" // Name of the revision.
	ManagedByPiped    = "piped"
)

// MakeManagedByPipedSelector returns a label selector that matches
// only resources managed by this piped instance.
func MakeManagedByPipedSelector() string {
	return fmt.Sprintf("%s=%s", LabelManagedBy, ManagedByPiped)
}

// MakeApplicationSelector returns a label selector that matches only
// resources managed by piped and belonging to the given application.
func MakeApplicationSelector(appID string) string {
	return fmt.Sprintf("%s=%s,%s=%s", LabelManagedBy, ManagedByPiped, LabelApplication, appID)
}

// MakeRevisionNamesSelector returns a label selector that matches
// revisions with any of the given names.
func MakeRevisionNamesSelector(names []string) string {
	return fmt.Sprintf("%s in (%s)", LabelRevisionName, strings.Join(names, ","))
}

// ServiceManifest converts a live Service object fetched from Cloud Run API
// back into a ServiceManifest so we can read and modify its fields.
func (s *Service) ServiceManifest() (ServiceManifest, error) {
	r := (*run.Service)(s)
	data, err := r.MarshalJSON()
	if err != nil {
		return ServiceManifest{}, err
	}
	return ParseServiceManifest(data)
}

// UID returns the unique ID assigned by Cloud Run to this service.
func (s *Service) UID() (string, bool) {
	if s.Metadata == nil || s.Metadata.Uid == "" {
		return "", false
	}
	return s.Metadata.Uid, true
}

// ActiveRevisionNames returns the names of all revisions currently
// receiving traffic on this service.
func (s *Service) ActiveRevisionNames() []string {
	if s.Status == nil {
		return nil
	}
	tf := s.Status.Traffic
	ret := make([]string, len(tf))
	for i := range tf {
		ret[i] = tf[i].RevisionName
	}
	return ret
}

// StatusConditions holds the parsed health conditions for a Service or Revision.
type StatusConditions struct {
	Kind      Kind
	TrueTypes map[string]struct{}

	// Deduplicated error and warning messages from the conditions.
	FalseMessages   []string
	UnknownMessages []string
}

// Kind identifies whether the conditions belong to a Service or a Revision.
type Kind string

const (
	KindService  Kind = "Service"
	KindRevision Kind = "Revision"
)

// TypeConditions is the full set of condition types we care about.
var TypeConditions = map[string]struct{}{
	"Active":              {},
	"Ready":               {},
	"ConfigurationsReady": {},
	"RoutesReady":         {},
	"ContainerHealthy":    {},
	"ResourcesAvailable":  {},
}

// TypeHealthyServiceConditions are the conditions that must all be True
// for a Service to be considered healthy.
var TypeHealthyServiceConditions = map[string]struct{}{
	"Ready":               {},
	"ConfigurationsReady": {},
	"RoutesReady":         {},
}

// TypeHealthyRevisionConditions are the conditions that must all be True
// for a Revision to be considered healthy.
var TypeHealthyRevisionConditions = map[string]struct{}{
	"Ready":              {},
	"Active":             {},
	"ContainerHealthy":   {},
	"ResourcesAvailable": {},
}

// StatusConditions parses the raw conditions from a live Service
// and returns a structured summary of its health state.
func (s *Service) StatusConditions() *StatusConditions {
	var (
		trueTypes   = make(map[string]struct{}, len(TypeConditions))
		falseMsgs   = make(map[string]string, len(TypeConditions))
		unknownMsgs = make(map[string]string, len(TypeConditions))
	)

	if s.Status == nil {
		return nil
	}
	for _, cond := range s.Status.Conditions {
		if _, ok := TypeConditions[cond.Type]; !ok {
			continue
		}
		switch cond.Status {
		case "True":
			trueTypes[cond.Type] = struct{}{}
		case "False":
			falseMsgs[cond.Reason] = cond.Message
		default:
			unknownMsgs[cond.Reason] = cond.Message
		}
	}

	fMsgs := make([]string, 0, len(falseMsgs))
	for _, v := range falseMsgs {
		fMsgs = append(fMsgs, v)
	}

	uMsgs := make([]string, 0, len(unknownMsgs))
	for _, v := range unknownMsgs {
		uMsgs = append(uMsgs, v)
	}

	return &StatusConditions{
		Kind:            KindService,
		TrueTypes:       trueTypes,
		FalseMessages:   fMsgs,
		UnknownMessages: uMsgs,
	}
}

// StatusConditions parses the raw conditions from a live Revision
// and returns a structured summary of its health state.
func (r *Revision) StatusConditions() *StatusConditions {
	var (
		trueTypes   = make(map[string]struct{}, len(TypeConditions))
		falseMsgs   = make(map[string]string, len(TypeConditions))
		unknownMsgs = make(map[string]string, len(TypeConditions))
	)

	if r.Status == nil {
		return nil
	}
	for _, cond := range r.Status.Conditions {
		if _, ok := TypeConditions[cond.Type]; !ok {
			continue
		}
		switch cond.Status {
		case "True":
			trueTypes[cond.Type] = struct{}{}
		case "False":
			falseMsgs[cond.Reason] = cond.Message
		default:
			unknownMsgs[cond.Reason] = cond.Message
		}
	}

	fMsgs := make([]string, 0, len(falseMsgs))
	for _, v := range falseMsgs {
		fMsgs = append(fMsgs, v)
	}

	uMsgs := make([]string, 0, len(unknownMsgs))
	for _, v := range unknownMsgs {
		uMsgs = append(uMsgs, v)
	}

	return &StatusConditions{
		Kind:            KindRevision,
		TrueTypes:       trueTypes,
		FalseMessages:   fMsgs,
		UnknownMessages: uMsgs,
	}
}

// HealthStatus translates the parsed conditions into a PipeCD health status,
// together with a human-readable description explaining why.
// Returns HEALTHY only if all required conditions are True.
// Returns OTHER if any condition is explicitly False (error).
// Returns UNKNOWN if any condition is unclear or missing.
func (s *StatusConditions) HealthStatus() (status string, desc string) {
	if s == nil {
		return "UNKNOWN", ""
	}
	if len(s.FalseMessages) > 0 {
		return "OTHER", strings.Join(s.FalseMessages, "; ")
	}
	if len(s.UnknownMessages) > 0 {
		return "UNKNOWN", strings.Join(s.UnknownMessages, "; ")
	}

	mustPassConditions := TypeHealthyServiceConditions
	if s.Kind == KindRevision {
		mustPassConditions = TypeHealthyRevisionConditions
	}
	for k := range mustPassConditions {
		if _, ok := s.TrueTypes[k]; !ok {
			return "UNKNOWN", fmt.Sprintf("condition %q is missing", k)
		}
	}
	return "HEALTHY", ""
}

// UID returns the unique ID assigned by Cloud Run to this revision.
func (r *Revision) UID() (string, bool) {
	if r.Metadata == nil || r.Metadata.Uid == "" {
		return "", false
	}
	return r.Metadata.Uid, true
}

// Name returns the name of this revision.
func (r *Revision) Name() string {
	if r.Metadata == nil {
		return ""
	}
	return r.Metadata.Name
}

// Labels returns the labels attached to this revision.
func (r *Revision) Labels() map[string]string {
	if r.Metadata == nil {
		return nil
	}
	return r.Metadata.Labels
}

// CreatedAt returns the time this revision was created.
// It returns the zero time if the timestamp is missing or cannot be parsed.
func (r *Revision) CreatedAt() time.Time {
	if r.Metadata == nil || r.Metadata.CreationTimestamp == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, r.Metadata.CreationTimestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}

// CreatedAt returns the time this service was created.
// It returns the zero time if the timestamp is missing or cannot be parsed.
func (s *Service) CreatedAt() time.Time {
	if s.Metadata == nil || s.Metadata.CreationTimestamp == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s.Metadata.CreationTimestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}
