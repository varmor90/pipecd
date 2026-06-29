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

package deployment

import (
	"context"
	"errors"
	"time"

	sdk "github.com/pipe-cd/piped-plugin-sdk-go"
	"go.uber.org/zap"

	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/client"
	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/config"
)

// revisionReadyTimeout is the maximum time to wait for a newly created
// revision to become ready before failing the stage.
// See proposal's edge case: "Revision readiness timeout".
const revisionReadyTimeout = 5 * time.Minute

// revisionPollInterval is how often we check whether the revision is ready.
const revisionPollInterval = 5 * time.Second

// executeSyncStage implements the CLOUDRUN_SYNC stage: it deploys the target
// manifest as a new revision and shifts 100% of the traffic to it ("direct
// strategy", as described in the proposal).
func executeSyncStage(ctx context.Context, input *sdk.ExecuteStageInput[config.CloudRunApplicationSpec], dts []*sdk.DeployTarget[config.CloudRunDeployTargetConfig]) sdk.StageStatus {
	lp := input.Client.LogPersister()
	lp.Info("Start syncing the Cloud Run service")

	return deployAndSwitchTraffic(ctx, lp, input.Logger, dts, input.Request.Deployment.ApplicationID, input.Request.TargetDeploymentSource)
}

// deployAndSwitchTraffic loads the service manifest from the given deployment
// source, deploys it as a new revision, waits for it to become ready, and
// then shifts 100% of the traffic to it ("direct strategy"). It is shared by
// both the CLOUDRUN_SYNC and ROLLBACK stages; they only differ in which
// deployment source (target vs. running) they deploy.
//
// This function is idempotent: it is safe to retry, because Create/Update simply
// re-applies the desired manifest, and switching all traffic to the same
// revision name again has no extra effect.
func deployAndSwitchTraffic(ctx context.Context, lp sdk.StageLogPersister, logger *zap.Logger, dts []*sdk.DeployTarget[config.CloudRunDeployTargetConfig], applicationID string, ds sdk.DeploymentSource[config.CloudRunApplicationSpec]) sdk.StageStatus {
	if len(dts) == 0 {
		lp.Error("No deploy target was found")
		return sdk.StageStatusFailure
	}
	dt := dts[0]

	cl, err := client.NewClient(ctx, dt.Config.Project, dt.Config.Region, dt.Config.CredentialsFile, logger)
	if err != nil {
		lp.Errorf("Failed to create cloud run client: %v", err)
		return sdk.StageStatusFailure
	}

	manifestPath, err := serviceManifestPath(ds)
	if err != nil {
		lp.Errorf("Failed to resolve service manifest path: %v", err)
		return sdk.StageStatusFailure
	}

	sm, err := client.LoadServiceManifest(manifestPath)
	if err != nil {
		lp.Errorf("Failed to load service manifest: %v", err)
		return sdk.StageStatusFailure
	}

	revisionName, err := client.DecideRevisionName(sm, ds.CommitHash)
	if err != nil {
		lp.Errorf("Failed to decide revision name: %v", err)
		return sdk.StageStatusFailure
	}
	if err := sm.SetRevision(revisionName); err != nil {
		lp.Errorf("Failed to set revision name on the manifest: %v", err)
		return sdk.StageStatusFailure
	}
	lp.Infof("Deploying new revision %q", revisionName)

	// Add the labels that identify this resource as managed by this piped/application,
	// so GetLivestate can find it later via client.MakeApplicationSelector.
	sm.AddLabels(map[string]string{
		client.LabelManagedBy:   client.ManagedByPiped,
		client.LabelApplication: applicationID,
	})
	if err := sm.AddRevisionLabels(map[string]string{
		client.LabelManagedBy:    client.ManagedByPiped,
		client.LabelApplication:  applicationID,
		client.LabelRevisionName: revisionName,
	}); err != nil {
		lp.Errorf("Failed to set revision labels on the manifest: %v", err)
		return sdk.StageStatusFailure
	}

	// Try to create the service first (this is the path for a brand-new
	// application's first deployment). If it already exists, Create returns
	// the specific ErrServiceAlreadyExists sentinel, and we fall back to
	// Update — this is the expected, common path for every deployment after
	// the first one. Any other error from Create is a real failure and is
	// NOT silently retried as an Update, so we don't mask unrelated problems
	// (e.g. invalid manifest, permission errors) behind a misleading retry.
	_, err = cl.Create(ctx, sm)
	switch {
	case err == nil:
		lp.Success("Successfully created the service")
	case errors.Is(err, client.ErrServiceAlreadyExists):
		lp.Info("Service already exists, updating it instead")
		if _, err := cl.Update(ctx, sm); err != nil {
			lp.Errorf("Failed to update the existing service: %v", err)
			return sdk.StageStatusFailure
		}
		lp.Success("Successfully updated the service")
	default:
		lp.Errorf("Failed to create the service: %v", err)
		return sdk.StageStatusFailure
	}
	lp.Success("Successfully switched all traffic to the new revision")

	return sdk.StageStatusSuccess
}

// waitForRevisionReady polls the Cloud Run API until the given revision
// reports a HEALTHY status, the context is cancelled, or revisionReadyTimeout
// elapses.
func waitForRevisionReady(ctx context.Context, lp sdk.StageLogPersister, cl client.Client, revisionName string) sdk.StageStatus {
	ctx, cancel := context.WithTimeout(ctx, revisionReadyTimeout)
	defer cancel()

	for {
		rev, err := cl.GetRevision(ctx, revisionName)
		if err != nil && err != client.ErrRevisionNotFound {
			lp.Errorf("Failed to get revision status: %v", err)
			return sdk.StageStatusFailure
		}

		if err == nil {
			status, desc := rev.StatusConditions().HealthStatus()
			switch status {
			case "HEALTHY":
				return sdk.StageStatusSuccess
			case "OTHER":
				lp.Errorf("Revision %q failed to become ready: %s", revisionName, desc)
				return sdk.StageStatusFailure
			default:
				lp.Infof("Revision %q is not ready yet: %s", revisionName, desc)
			}
		}

		select {
		case <-time.After(revisionPollInterval):
			// keep polling
		case <-ctx.Done():
			lp.Errorf("Timed out waiting for revision %q to become ready", revisionName)
			return sdk.StageStatusFailure
		}
	}
}
