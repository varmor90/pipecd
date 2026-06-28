// Copyright 2025 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package livestate

import (
	"context"
	"fmt"

	sdk "github.com/pipe-cd/piped-plugin-sdk-go"

	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/client"
	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/config"
)

type Plugin struct{}

func (p Plugin) GetLivestate(ctx context.Context, _ *sdk.ConfigNone, dts []*sdk.DeployTarget[config.CloudRunDeployTargetConfig], input *sdk.GetLivestateInput[config.CloudRunApplicationSpec]) (*sdk.GetLivestateResponse, error) {
	if len(dts) == 0 {
		return nil, fmt.Errorf("no deploy target was given")
	}
	dt := dts[0]

	cl, err := client.NewClient(ctx, dt.Config.Project, dt.Config.Region, dt.Config.CredentialsFile, input.Logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create cloud run client: %w", err)
	}

	selector := client.MakeApplicationSelector(input.Request.ApplicationID)

	svcs, _, err := cl.List(ctx, &client.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("failed to list cloud run services: %w", err)
	}
	if len(svcs) == 0 {
		// No service found yet (e.g. not deployed). Report an empty live state
		// instead of an error so the application shows up as having no resources.
		return &sdk.GetLivestateResponse{
			LiveState: sdk.ApplicationLiveState{Resources: []sdk.ResourceState{}},
			SyncState: sdk.ApplicationSyncState{Status: sdk.ApplicationSyncStateUnknown},
		}, nil
	}
	svc := svcs[0]

	revs, _, err := cl.ListRevisions(ctx, &client.ListRevisionsOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("failed to list cloud run revisions: %w", err)
	}

	resources := make([]sdk.ResourceState, 0, len(revs)+1)

	// Service resource state.
	if rs := serviceResourceState(svc, dt.Name); rs != nil {
		resources = append(resources, *rs)
	}

	// Revision resource states.
	for _, rev := range revs {
		if rs := revisionResourceState(rev, svc, dt.Name); rs != nil {
			resources = append(resources, *rs)
		}
	}

	return &sdk.GetLivestateResponse{
		LiveState: sdk.ApplicationLiveState{Resources: resources},
		SyncState: sdk.ApplicationSyncState{Status: sdk.ApplicationSyncStateUnknown},
	}, nil
}

// serviceResourceState converts a live Cloud Run Service into an sdk.ResourceState.
func serviceResourceState(svc *client.Service, deployTarget string) *sdk.ResourceState {
	uid, ok := svc.UID()
	if !ok {
		return nil
	}

	status, desc := svc.StatusConditions().HealthStatus()

	return &sdk.ResourceState{
		ID:                uid,
		ParentIDs:         nil,
		Name:              svc.Metadata.Name,
		ResourceType:      "Service",
		ResourceMetadata:  map[string]string{},
		HealthStatus:      toSDKHealthStatus(status),
		HealthDescription: desc,
		DeployTarget:      deployTarget,
		CreatedAt:         svc.CreatedAt(),
	}
}

// revisionResourceState converts a live Cloud Run Revision into an sdk.ResourceState.
// The owning service's UID is set as the revision's parent.
func revisionResourceState(rev *client.Revision, svc *client.Service, deployTarget string) *sdk.ResourceState {
	uid, ok := rev.UID()
	if !ok {
		return nil
	}

	var parentIDs []string
	if svcUID, ok := svc.UID(); ok {
		parentIDs = []string{svcUID}
	}

	status, desc := rev.StatusConditions().HealthStatus()

	return &sdk.ResourceState{
		ID:                uid,
		ParentIDs:         parentIDs,
		Name:              rev.Name(),
		ResourceType:      "Revision",
		ResourceMetadata:  map[string]string{},
		HealthStatus:      toSDKHealthStatus(status),
		HealthDescription: desc,
		DeployTarget:      deployTarget,
		CreatedAt:         rev.CreatedAt(),
	}
}

// toSDKHealthStatus translates our own "HEALTHY"/"OTHER"/"UNKNOWN" status
// string into the SDK's 3-state sdk.ResourceHealthStatus.
func toSDKHealthStatus(status string) sdk.ResourceHealthStatus {
	switch status {
	case "HEALTHY":
		return sdk.ResourceHealthStateHealthy
	case "OTHER":
		return sdk.ResourceHealthStateUnhealthy
	default:
		return sdk.ResourceHealthStateUnknown
	}
}
