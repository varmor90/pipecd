package cloudrun

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"go.uber.org/zap"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/run/v1"
)

// Sentinel errors returned when a resource is not found in Cloud Run.
// Callers can check for these to handle "not found" cases explicitly.
var (
	ErrServiceNotFound  = fmt.Errorf("service not found")
	ErrRevisionNotFound = fmt.Errorf("revision not found")
)

// client holds the connection details and the underlying Cloud Run API client.
type client struct {
	projectID string
	region    string
	client    *run.APIService
	logger    *zap.Logger
}

// Client defines the operations the plugin needs to perform on Cloud Run.
// This interface makes it easy to swap the real client for a fake one in tests.
type Client interface {
	Create(ctx context.Context, sm ServiceManifest) (*Service, error)
	Update(ctx context.Context, sm ServiceManifest) (*Service, error)
	List(ctx context.Context, options *ListOptions) ([]*Service, string, error)
	GetRevision(ctx context.Context, name string) (*Revision, error)
	ListRevisions(ctx context.Context, options *ListRevisionsOptions) ([]*Revision, string, error)
}

// ListOptions controls filtering and pagination when listing services.
type ListOptions struct {
	Limit         int64
	LabelSelector string
	Cursor        string // Pagination token from a previous List call.
}

// ListRevisionsOptions controls filtering and pagination when listing revisions.
type ListRevisionsOptions struct {
	Limit         int64
	LabelSelector string
	Cursor        string // Pagination token from a previous ListRevisions call.
}

// newClient opens a connection to the Cloud Run API for the given project and region.
// If credentialsFile is provided, it is used for authentication; otherwise
// Application Default Credentials are used.
func newClient(ctx context.Context, projectID, region, credentialsFile string, logger *zap.Logger) (*client, error) {
	c := &client{
		projectID: projectID,
		region:    region,
		logger:    logger.Named("cloudrun"),
	}

	var options []option.ClientOption
	if len(credentialsFile) > 0 {
		data, err := os.ReadFile(credentialsFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read credentials file (%w)", err)
		}
		options = append(options, option.WithCredentialsJSON(data))
	}
	// Point the client at the regional Cloud Run endpoint.
	options = append(options,
		option.WithEndpoint(fmt.Sprintf("https://%s-run.googleapis.com/", region)),
	)

	runClient, err := run.NewService(ctx, options...)
	if err != nil {
		return nil, err
	}
	c.client = runClient

	return c, nil
}

// Create deploys a brand-new Cloud Run service from the given manifest.
// Used only on the very first deployment of an application.
func (c *client) Create(ctx context.Context, sm ServiceManifest) (*Service, error) {
	// Convert our manifest into the type expected by the Google API.
	svcCfg, err := sm.RunService()
	if err != nil {
		return nil, err
	}

	var (
		svc    = run.NewNamespacesServicesService(c.client)
		parent = makeCloudRunParent(c.projectID)
		call   = svc.Create(parent, svcCfg)
	)
	call.Context(ctx)

	service, err := call.Do()
	if err != nil {
		if e, ok := err.(*googleapi.Error); ok {
			return nil, fmt.Errorf("failed to create service: code=%d, message=%s, details=%s", e.Code, e.Message, e.Details)
		}
		return nil, err
	}
	return (*Service)(service), nil
}

// Update replaces an existing Cloud Run service with the new manifest.
// Used on every subsequent deployment after the service already exists.
func (c *client) Update(ctx context.Context, sm ServiceManifest) (*Service, error) {
	svcCfg, err := sm.RunService()
	if err != nil {
		return nil, err
	}

	var (
		svc  = run.NewNamespacesServicesService(c.client)
		name = makeCloudRunServiceName(c.projectID, sm.Name)
		call = svc.ReplaceService(name, svcCfg)
	)
	call.Context(ctx)

	service, err := call.Do()
	if err != nil {
		// Translate 404 into a typed error so callers can react to it.
		if e, ok := err.(*googleapi.Error); ok && e.Code == http.StatusNotFound {
			return nil, ErrServiceNotFound
		}
		return nil, err
	}
	return (*Service)(service), nil
}

// List returns all Cloud Run services in the project.
// Results are paginated; the returned cursor can be passed in options.Cursor
// on the next call to fetch the following page. An empty cursor means no more pages.
func (c *client) List(ctx context.Context, options *ListOptions) ([]*Service, string, error) {
	var (
		svc    = run.NewNamespacesServicesService(c.client)
		parent = makeCloudRunParent(c.projectID)
		call   = svc.List(parent)
	)
	call.Context(ctx)
	if options.Limit != 0 {
		call.Limit(options.Limit)
	}
	if options.LabelSelector != "" {
		call.LabelSelector(options.LabelSelector)
	}
	if options.Cursor != "" {
		call.Continue(options.Cursor)
	}

	resp, err := call.Do()
	if err != nil {
		return nil, "", err
	}

	var cursor string
	if resp.Metadata != nil {
		cursor = resp.Metadata.Continue
	}

	svcs := make([]*Service, 0, len(resp.Items))
	for i := range resp.Items {
		svc := (*Service)(resp.Items[i])
		svcs = append(svcs, svc)
	}
	return svcs, cursor, nil
}

// GetRevision fetches a single revision by name.
// Returns ErrRevisionNotFound if the revision does not exist.
func (c *client) GetRevision(ctx context.Context, name string) (*Revision, error) {
	var (
		svc  = run.NewNamespacesRevisionsService(c.client)
		id   = makeCloudRunRevisionName(c.projectID, name)
		call = svc.Get(id)
	)
	call.Context(ctx)

	revision, err := call.Do()
	if err != nil {
		if e, ok := err.(*googleapi.Error); ok && e.Code == http.StatusNotFound {
			return nil, ErrRevisionNotFound
		}
		return nil, err
	}
	return (*Revision)(revision), nil
}

// ListRevisions returns all revisions in the project.
// Supports the same pagination and filtering as List.
func (c *client) ListRevisions(ctx context.Context, options *ListRevisionsOptions) ([]*Revision, string, error) {
	var (
		rev    = run.NewNamespacesRevisionsService(c.client)
		parent = makeCloudRunParent(c.projectID)
		call   = rev.List(parent)
	)
	call.Context(ctx)
	if options.Limit != 0 {
		call.Limit(options.Limit)
	}
	if options.LabelSelector != "" {
		call.LabelSelector(options.LabelSelector)
	}
	if options.Cursor != "" {
		call.Continue(options.Cursor)
	}

	resp, err := call.Do()
	if err != nil {
		return nil, "", err
	}

	var cursor string
	if resp.Metadata != nil {
		cursor = resp.Metadata.Continue
	}

	revs := make([]*Revision, 0, len(resp.Items))
	for i := range resp.Items {
		rev := (*Revision)(resp.Items[i])
		revs = append(revs, rev)
	}
	return revs, cursor, nil
}

// makeCloudRunParent returns the namespace path for a GCP project.
// Cloud Run API uses "namespaces/{projectID}" to identify a project.
func makeCloudRunParent(projectID string) string {
	return fmt.Sprintf("namespaces/%s", projectID)
}

// makeCloudRunServiceName returns the full resource path for a Cloud Run service.
func makeCloudRunServiceName(projectID, serviceID string) string {
	return fmt.Sprintf("namespaces/%s/services/%s", projectID, serviceID)
}

// makeCloudRunRevisionName returns the full resource path for a Cloud Run revision.
func makeCloudRunRevisionName(projectID, revisionID string) string {
	return fmt.Sprintf("namespaces/%s/revisions/%s", projectID, revisionID)
}
