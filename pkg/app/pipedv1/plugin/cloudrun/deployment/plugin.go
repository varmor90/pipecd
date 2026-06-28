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
	"fmt"
	"path/filepath"
	"slices"

	sdk "github.com/pipe-cd/piped-plugin-sdk-go"

	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/client"
	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/config"
)

type Plugin struct{}

const (
	// StageCloudRunSync does quick sync by rolling out the new version
	// and switching all traffic to it.
	StageCloudRunSync = "CLOUDRUN_SYNC"
	// StageCloudRunPromote promotes the new version to receive amount of traffic.
	StageCloudRunPromote = "CLOUDRUN_PROMOTE"
	// StageRollback the legacy generic rollback stage name
	StageRollback = "ROLLBACK"

	StageCloudRunSyncDescription = "Deploy the new version and configure all traffic to it"
	StageRollbackDescription     = "Rollback the deployment"
)

func (p *Plugin) FetchDefinedStages() []string {
	return []string{StageCloudRunSync, StageCloudRunPromote, StageRollback}
}

func (p *Plugin) BuildPipelineSyncStages(ctx context.Context, _ *sdk.ConfigNone, input *sdk.BuildPipelineSyncStagesInput) (*sdk.BuildPipelineSyncStagesResponse, error) {
	return &sdk.BuildPipelineSyncStagesResponse{
		Stages: buildPipelineStages(input.Request.Stages, input.Request.Rollback),
	}, nil
}

func (p *Plugin) ExecuteStage(ctx context.Context, _ *sdk.ConfigNone, dts []*sdk.DeployTarget[config.CloudRunDeployTargetConfig], input *sdk.ExecuteStageInput[config.CloudRunApplicationSpec]) (*sdk.ExecuteStageResponse, error) {
	var status sdk.StageStatus

	switch input.Request.StageName {
	case StageCloudRunSync:
		status = executeSyncStage(ctx, input, dts)
	case StageRollback:
		status = executeRollbackStage(ctx, input, dts)
	case StageCloudRunPromote:
		input.Client.LogPersister().Error("CLOUDRUN_PROMOTE is not implemented yet")
		status = sdk.StageStatusFailure
	default:
		input.Client.LogPersister().Errorf("Unsupported stage %q", input.Request.StageName)
		status = sdk.StageStatusFailure
	}

	return &sdk.ExecuteStageResponse{Status: status}, nil
}

func (p *Plugin) DetermineVersions(ctx context.Context, _ *sdk.ConfigNone, input *sdk.DetermineVersionsInput[config.CloudRunApplicationSpec]) (*sdk.DetermineVersionsResponse, error) {
	manifestPath, err := serviceManifestPath(input.Request.DeploymentSource)
	if err != nil {
		return nil, err
	}

	sm, err := client.LoadServiceManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load service manifest: %w", err)
	}

	versions, err := client.FindArtifactVersions(sm)
	if err != nil {
		return nil, fmt.Errorf("failed to find artifact versions: %w", err)
	}

	return &sdk.DetermineVersionsResponse{Versions: versions}, nil
}

// serviceManifestPath builds the full path to the service manifest file
// (e.g. service.yaml) for the given deployment source, falling back to
// the default filename if none was set in the application config.
func serviceManifestPath(ds sdk.DeploymentSource[config.CloudRunApplicationSpec]) (string, error) {
	if ds.ApplicationConfig == nil || ds.ApplicationConfig.Spec == nil {
		return "", fmt.Errorf("application config spec is missing")
	}

	filename := ds.ApplicationConfig.Spec.Input.ServiceManifestFile
	if filename == "" {
		filename = client.DefaultServiceManifestFilename
	}

	return filepath.Join(ds.ApplicationDirectory, filename), nil
}

// DetermineStrategy intentionally has no Cloud Run-specific logic: deployment
// decisions belong to the core, not the plugin (see proposal's separation of
// concerns). Returning (nil, nil) tells the core to fall back to its own
// default logic (PipelineSync) for choosing the deployment strategy.
func (p *Plugin) DetermineStrategy(ctx context.Context, _ *sdk.ConfigNone, input *sdk.DetermineStrategyInput[config.CloudRunApplicationSpec]) (*sdk.DetermineStrategyResponse, error) {
	return nil, nil
}

func (p *Plugin) BuildQuickSyncStages(ctx context.Context, _ *sdk.ConfigNone, input *sdk.BuildQuickSyncStagesInput) (*sdk.BuildQuickSyncStagesResponse, error) {
	return &sdk.BuildQuickSyncStagesResponse{
		Stages: buildQuickSyncPipeline(input.Request.Rollback),
	}, nil
}

func buildQuickSyncPipeline(autoRollback bool) []sdk.QuickSyncStage {
	out := make([]sdk.QuickSyncStage, 0, 2)
	out = append(out, sdk.QuickSyncStage{
		Name:               StageCloudRunSync,
		Description:        StageCloudRunSyncDescription,
		Rollback:           false,
		Metadata:           map[string]string{},
		AvailableOperation: sdk.ManualOperationNone,
	})
	if autoRollback {
		out = append(out, sdk.QuickSyncStage{
			Name:               StageRollback,
			Description:        StageRollbackDescription,
			Rollback:           true,
			Metadata:           map[string]string{},
			AvailableOperation: sdk.ManualOperationNone,
		})
	}
	return out
}

func buildPipelineStages(stages []sdk.StageConfig, autoRollback bool) []sdk.PipelineStage {
	out := make([]sdk.PipelineStage, 0, len(stages)+1)
	for _, stage := range stages {
		out = append(out, sdk.PipelineStage{
			Name:               stage.Name,
			Index:              stage.Index,
			Rollback:           false,
			Metadata:           map[string]string{},
			AvailableOperation: sdk.ManualOperationNone,
		})
	}
	if autoRollback {
		out = append(out, sdk.PipelineStage{
			Name: StageRollback,
			Index: slices.MinFunc(stages, func(a, b sdk.StageConfig) int {
				return a.Index - b.Index
			}).Index,
			Rollback:           true,
			Metadata:           map[string]string{},
			AvailableOperation: sdk.ManualOperationNone,
		})
	}
	return out
}
