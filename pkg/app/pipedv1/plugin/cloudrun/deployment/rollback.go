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

	sdk "github.com/pipe-cd/piped-plugin-sdk-go"

	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/config"
)

// executeRollbackStage implements the ROLLBACK stage: it re-deploys the
// previously running manifest (RunningDeploymentSource) and shifts all
// traffic back to it, undoing a failed or unwanted deployment.
func executeRollbackStage(ctx context.Context, input *sdk.ExecuteStageInput[config.CloudRunApplicationSpec], dts []*sdk.DeployTarget[config.CloudRunDeployTargetConfig]) sdk.StageStatus {
	lp := input.Client.LogPersister()
	lp.Info("Start rolling back the Cloud Run service to the previous version")

	return deployAndSwitchTraffic(ctx, lp, input.Logger, dts, input.Request.Deployment.ApplicationID, input.Request.RunningDeploymentSource)
}
