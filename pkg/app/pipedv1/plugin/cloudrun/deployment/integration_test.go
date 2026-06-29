//go:build integration

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

// This file contains an integration test that talks to the REAL Cloud Run
// API. It is intentionally excluded from a normal `go test ./...` run by
// the "integration" build tag at the top of this file — running it requires
// real GCP credentials and creates real (billable, though free-tier-eligible)
// cloud resources, which is not something we want happening automatically
// on every build or in someone else's CI pipeline.
//
// To run this test deliberately:
//
//	go test -tags=integration ./deployment/... -run TestIntegration -v
//
// It expects the following environment variables to be set beforehand
// (see README/NOTES for how to obtain them):
//
//	CLOUDRUN_TEST_PROJECT=my-gcp-project
//	CLOUDRUN_TEST_REGION=europe-west1
//	CLOUDRUN_TEST_CREDENTIALS_FILE=C:\path\to\service-account-key.json
//	CLOUDRUN_TEST_APP_DIR=C:\path\to\folder\containing\service.yaml
package deployment

import (
	"context"
	"os"
	"testing"
	"time"

	sdk "github.com/pipe-cd/piped-plugin-sdk-go"
	"go.uber.org/zap"

	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/client"
	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/config"
)

// testLogPersister is a stand-in for the real sdk.StageLogPersister.
//
// In production, log messages from a deployment stage (Info/Success/Error)
// are streamed to piped over gRPC and shown in the PipeCD UI. In this test
// there is no real piped process running, so instead we just forward every
// log line to Go's test logger (t.Log), which prints it in the `go test -v`
// output. This lets us see the exact same progress messages a real user
// would see in the UI, without needing a live PipeCD server.
type testLogPersister struct {
	t *testing.T
}

// Write implements the io.Writer-like part of the interface. Stage output
// that isn't a structured Info/Success/Error call goes through here.
func (p *testLogPersister) Write(log []byte) (int, error) {
	p.t.Log(string(log))
	return len(log), nil
}

func (p *testLogPersister) Info(log string) { p.t.Log("[INFO] " + log) }

func (p *testLogPersister) Infof(format string, a ...interface{}) {
	p.t.Logf("[INFO] "+format, a...)
}

func (p *testLogPersister) Success(log string) { p.t.Log("[SUCCESS] " + log) }

func (p *testLogPersister) Successf(format string, a ...interface{}) {
	p.t.Logf("[SUCCESS] "+format, a...)
}

func (p *testLogPersister) Error(log string) { p.t.Log("[ERROR] " + log) }

func (p *testLogPersister) Errorf(format string, a ...interface{}) {
	p.t.Logf("[ERROR] "+format, a...)
}

// requireEnv reads a required environment variable for this test. If it's
// missing, the test is skipped (not failed) with a clear explanation —
// this is what makes it safe for this test to sit in the same package as
// the normal unit tests without breaking anyone else's `go test ./...`.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("skipping integration test: environment variable %s is not set", key)
	}
	return v
}

// TestIntegration_ExecuteSyncStage exercises the full CLOUDRUN_SYNC code
// path against a real Cloud Run service:
//
//  1. Deploy a new revision built from a real service.yaml manifest.
//  2. Wait for that revision to become healthy.
//  3. Shift 100% of traffic to it.
//  4. Confirm the service can be found again via the same label selector
//     that GetLivestate uses, proving the labels we attach during the
//     deploy are consistent with how we look resources up afterwards.
//
// This single test exercises almost every piece of client/ that the
// earlier, smaller integration test (client/integration_test.go) did not
// already cover: Update, UpdateAllTraffic, SetRevision, DecideRevisionName,
// and the labelling helpers.
func TestIntegration_ExecuteSyncStage(t *testing.T) {
	project := requireEnv(t, "CLOUDRUN_TEST_PROJECT")
	region := requireEnv(t, "CLOUDRUN_TEST_REGION")
	credentialsFile := requireEnv(t, "CLOUDRUN_TEST_CREDENTIALS_FILE")
	appDir := requireEnv(t, "CLOUDRUN_TEST_APP_DIR")

	const (
		// testAppID is a fake PipeCD application ID. It only needs to be
		// consistent within this test: it's what we attach as a label
		// during deploy, and what we filter by when verifying afterwards.
		testAppID = "test-app-id"
	)

	// Give the whole test (deploy + wait-for-ready + traffic switch) a
	// generous timeout. waitForRevisionReady has its own internal 5-minute
	// timeout, so this outer one is just a safety net for the rest of the
	// test logic around it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// deployedServiceName is filled in once we know the real service name
	// (read from the manifest's metadata, not hardcoded), so the cleanup
	// message below always points at the service that was actually created.
	var deployedServiceName string

	// We don't call client.Delete here because the client package doesn't
	// implement it yet (see NOTES). Instead, print a copy-pasteable cleanup
	// command so nothing is silently left running in the GCP project.
	t.Cleanup(func() {
		if deployedServiceName == "" {
			t.Log("NOTE: no service was confirmed created, nothing to clean up")
			return
		}
		t.Logf("NOTE: clean up manually with: gcloud run services delete %s --project=%s --region=%s --quiet",
			deployedServiceName, project, region)
	})
	lp := &testLogPersister{t: t}

	// sdk.NewClient is explicitly documented in the SDK as "DO NOT USE this
	// function except in tests" — it's the SDK's own escape hatch for
	// exactly this situation. We pass `base = nil` (the real gRPC
	// connection to piped) because executeSyncStage never calls any method
	// on sdk.Client other than LogPersister(), which just returns the `lp`
	// we already constructed above — it never touches `base`.
	sdkClient := sdk.NewClient(nil, "cloudrun", testAppID, "test-stage-id", lp, nil)

	// DeployTarget carries the per-target Cloud Run connection details
	// (project, region, credentials file) that would normally come from
	// the piped's plugin configuration file.
	dts := []*sdk.DeployTarget[config.CloudRunDeployTargetConfig]{
		{
			Name: "default",
			Config: config.CloudRunDeployTargetConfig{
				Project:         project,
				Region:          region,
				CredentialsFile: credentialsFile,
			},
		},
	}

	// This mirrors what sdk.DeploymentPluginServiceServer would normally
	// build for us from a real gRPC request (see deployment.go in the SDK).
	// We build it by hand here since there's no live piped sending us that
	// request in a test.
	input := &sdk.ExecuteStageInput[config.CloudRunApplicationSpec]{
		Request: sdk.ExecuteStageRequest[config.CloudRunApplicationSpec]{
			StageName: StageCloudRunSync,
			Deployment: sdk.Deployment{
				ApplicationID: testAppID,
			},
			// TargetDeploymentSource points at the directory containing our
			// test service.yaml, plus the application config telling our
			// code what that manifest file is called.
			TargetDeploymentSource: sdk.DeploymentSource[config.CloudRunApplicationSpec]{
				ApplicationDirectory: appDir,
				CommitHash:           "test123",
				ApplicationConfig: &sdk.ApplicationConfig[config.CloudRunApplicationSpec]{
					// Only the public Spec field is set here. The SDK's
					// ApplicationConfig also has private fields
					// (commonSpec, pluginConfigs) used when parsing a full
					// app.pipecd.yaml from disk, but we verified that
					// nothing on this code path (serviceManifestPath,
					// deployAndSwitchTraffic) ever reads them — only
					// .Spec is used — so leaving them as zero-value here
					// is safe.
					Spec: &config.CloudRunApplicationSpec{
						Input: config.CloudRunDeploymentInput{
							ServiceManifestFile: "service.yaml",
						},
					},
				},
			},
		},
		Client: sdkClient,
		Logger: zap.NewNop(), // discard internal zap logs; we only care about lp output above
	}

	t.Log("Executing CLOUDRUN_SYNC stage...")
	status := executeSyncStage(ctx, input, dts)
	if status != sdk.StageStatusSuccess {
		t.Fatalf("expected StageStatusSuccess, got %v", status)
	}

	// Beyond just checking the stage reported success, independently
	// re-fetch the service through the same label selector GetLivestate
	// uses. This confirms the labels applied during deploy
	// (LabelManagedBy / LabelApplication) are actually queryable afterwards
	// — i.e. that ExecuteStage and GetLivestate agree on how to find the
	// same resource.
	t.Log("Verifying the service is reachable via the client...")
	cl, err := client.NewClient(ctx, project, region, credentialsFile, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create verification client: %v", err)
	}
	svcs, _, err := cl.List(ctx, &client.ListOptions{LabelSelector: client.MakeApplicationSelector(testAppID)})
	if err != nil {
		t.Fatalf("failed to list services: %v", err)
	}
	if len(svcs) != 1 {
		t.Fatalf("expected exactly 1 service matching the application selector, got %d", len(svcs))
	}
	deployedServiceName = svcs[0].Metadata.Name
	t.Logf("Found service %q with %d active revision(s)", deployedServiceName, len(svcs[0].ActiveRevisionNames()))

	t.Log("Integration test passed!")
}

// TestIntegration_ExecuteRollbackStage exercises the ROLLBACK code path.
// It first deploys a service via executeSyncStage (reusing the same
// manifest as a stand-in "previously running" version, since this test
// doesn't need two different container versions to prove the rollback
// mechanics work), and then calls executeRollbackStage with that same
// manifest as the RunningDeploymentSource, mimicking what a real rollback
// would do: redeploy the last known-good version and switch traffic back
// to it.
func TestIntegration_ExecuteRollbackStage(t *testing.T) {
	project := requireEnv(t, "CLOUDRUN_TEST_PROJECT")
	region := requireEnv(t, "CLOUDRUN_TEST_REGION")
	credentialsFile := requireEnv(t, "CLOUDRUN_TEST_CREDENTIALS_FILE")
	appDir := requireEnv(t, "CLOUDRUN_TEST_APP_DIR")

	const testAppID = "test-app-id-rollback"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var deployedServiceName string
	t.Cleanup(func() {
		if deployedServiceName == "" {
			t.Log("NOTE: no service was confirmed created, nothing to clean up")
			return
		}
		t.Logf("NOTE: clean up manually with: gcloud run services delete %s --project=%s --region=%s --quiet",
			deployedServiceName, project, region)
	})

	lp := &testLogPersister{t: t}
	sdkClient := sdk.NewClient(nil, "cloudrun", testAppID, "test-stage-id", lp, nil)

	dts := []*sdk.DeployTarget[config.CloudRunDeployTargetConfig]{
		{
			Name: "default",
			Config: config.CloudRunDeployTargetConfig{
				Project:         project,
				Region:          region,
				CredentialsFile: credentialsFile,
			},
		},
	}

	deploymentSource := sdk.DeploymentSource[config.CloudRunApplicationSpec]{
		ApplicationDirectory: appDir,
		CommitHash:           "rollbacktest",
		ApplicationConfig: &sdk.ApplicationConfig[config.CloudRunApplicationSpec]{
			Spec: &config.CloudRunApplicationSpec{
				Input: config.CloudRunDeploymentInput{
					ServiceManifestFile: "service.yaml",
				},
			},
		},
	}

	// Step 1: deploy the "previously running" version via a normal sync,
	// so there's something real to roll back to.
	t.Log("Step 1: deploying the initial version via CLOUDRUN_SYNC...")
	syncInput := &sdk.ExecuteStageInput[config.CloudRunApplicationSpec]{
		Request: sdk.ExecuteStageRequest[config.CloudRunApplicationSpec]{
			StageName:              StageCloudRunSync,
			Deployment:             sdk.Deployment{ApplicationID: testAppID},
			TargetDeploymentSource: deploymentSource,
		},
		Client: sdkClient,
		Logger: zap.NewNop(),
	}
	if status := executeSyncStage(ctx, syncInput, dts); status != sdk.StageStatusSuccess {
		t.Fatalf("initial sync failed with status %v, cannot test rollback", status)
	}

	// Step 2: roll back to that same manifest, exercising the
	// RunningDeploymentSource code path instead of TargetDeploymentSource.
	t.Log("Step 2: rolling back...")
	rollbackInput := &sdk.ExecuteStageInput[config.CloudRunApplicationSpec]{
		Request: sdk.ExecuteStageRequest[config.CloudRunApplicationSpec]{
			StageName:               StageRollback,
			Deployment:              sdk.Deployment{ApplicationID: testAppID},
			RunningDeploymentSource: deploymentSource,
		},
		Client: sdkClient,
		Logger: zap.NewNop(),
	}
	status := executeRollbackStage(ctx, rollbackInput, dts)
	if status != sdk.StageStatusSuccess {
		t.Fatalf("expected StageStatusSuccess from rollback, got %v", status)
	}

	// Verify the service still exists and is healthy after the rollback.
	t.Log("Verifying the service is still reachable after rollback...")
	cl, err := client.NewClient(ctx, project, region, credentialsFile, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create verification client: %v", err)
	}
	svcs, _, err := cl.List(ctx, &client.ListOptions{LabelSelector: client.MakeApplicationSelector(testAppID)})
	if err != nil {
		t.Fatalf("failed to list services: %v", err)
	}
	if len(svcs) != 1 {
		t.Fatalf("expected exactly 1 service matching the application selector, got %d", len(svcs))
	}
	deployedServiceName = svcs[0].Metadata.Name
	t.Logf("Found service %q with %d active revision(s) after rollback", deployedServiceName, len(svcs[0].ActiveRevisionNames()))

	t.Log("Rollback integration test passed!")
}
