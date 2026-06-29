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

// This file contains integration tests that talk to the real Cloud Run API.
// They are excluded from normal `go test ./...` runs via the "integration"
// build tag, because they require real GCP credentials and create real
// (billable) resources.
//
// To run these tests, set the following environment variables and run:
//
//	go test -tags=integration ./client/... -run TestIntegration -v
//
//	CLOUDRUN_TEST_PROJECT=my-gcp-project
//	CLOUDRUN_TEST_REGION=europe-west1
//	CLOUDRUN_TEST_CREDENTIALS_FILE=C:\path\to\key.json
package client_test

import (
	"context"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pipe-cd/pipecd/pkg/app/pipedv1/plugin/cloudrunservice/client"
)

// testServiceName is the name of the throwaway Cloud Run service created
// and destroyed by this test.
const testServiceName = "pipecd-integration-test"

// testServiceManifest is a minimal service manifest pointing at Google's
// public sample "hello" image, so the test doesn't need to build anything.
const testServiceManifestYAML = `
apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: ` + testServiceName + `
spec:
  template:
    metadata:
      name: ` + testServiceName + `-v1
    spec:
      containers:
        - image: gcr.io/cloudrun/hello
`

// requireEnv reads an environment variable required for the integration
// test, skipping the test with a clear message if it's not set.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("skipping integration test: environment variable %s is not set", key)
	}
	return v
}

func TestIntegration_CreateListGetRevision(t *testing.T) {
	project := requireEnv(t, "CLOUDRUN_TEST_PROJECT")
	region := requireEnv(t, "CLOUDRUN_TEST_REGION")
	credentialsFile := requireEnv(t, "CLOUDRUN_TEST_CREDENTIALS_FILE")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	logger := zap.NewNop()

	cl, err := client.NewClient(ctx, project, region, credentialsFile, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	sm, err := client.ParseServiceManifest([]byte(testServiceManifestYAML))
	if err != nil {
		t.Fatalf("ParseServiceManifest failed: %v", err)
	}

	// Clean up the service at the end of the test, regardless of outcome.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		deleteTestService(cleanupCtx, t, project, region, credentialsFile)
	})

	t.Log("Creating Cloud Run service...")
	svc, err := cl.Create(ctx, sm)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Logf("Created service, UID: %v", svc.Metadata.Uid)

	t.Log("Listing services...")
	svcs, _, err := cl.List(ctx, &client.ListOptions{})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	found := false
	for _, s := range svcs {
		if s.Metadata.Name == testServiceName {
			found = true
		}
	}
	if !found {
		t.Errorf("expected to find service %q in List results, but it wasn't there", testServiceName)
	}

	t.Log("Waiting for the revision to become ready...")
	revisionName := testServiceName + "-v1"
	deadline := time.Now().Add(2 * time.Minute)
	for {
		rev, err := cl.GetRevision(ctx, revisionName)
		if err == nil {
			status, desc := rev.StatusConditions().HealthStatus()
			t.Logf("Revision status: %s (%s)", status, desc)
			if status == "HEALTHY" {
				break
			}
			if status == "OTHER" {
				t.Fatalf("revision failed to become ready: %s", desc)
			}
		} else if err != client.ErrRevisionNotFound {
			t.Fatalf("GetRevision failed: %v", err)
		}

		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for revision to become ready")
		}
		time.Sleep(5 * time.Second)
	}

	t.Log("Integration test passed!")
}

// deleteTestService removes the Cloud Run service created by this test,
// using gcloud-equivalent low-level API calls is overkill here; instead we
// just log a reminder, since the client package doesn't expose Delete yet.
func deleteTestService(ctx context.Context, t *testing.T, project, region, credentialsFile string) {
	t.Helper()
	t.Logf("NOTE: test service %q was not deleted automatically (client.Delete is not implemented yet). "+
		"Clean it up manually with: gcloud run services delete %s --project=%s --region=%s --quiet",
		testServiceName, testServiceName, project, region)
}

// TestIntegration_CreateConflictErrorCode is a throwaway diagnostic test:
// it creates the same service twice in a row to discover exactly what
// HTTP status code and error shape Cloud Run returns on the second Create
// call. This informs how we distinguish "already exists" from other
// failures in deployAndSwitchTraffic.
func TestIntegration_CreateConflictErrorCode(t *testing.T) {
	project := requireEnv(t, "CLOUDRUN_TEST_PROJECT")
	region := requireEnv(t, "CLOUDRUN_TEST_REGION")
	credentialsFile := requireEnv(t, "CLOUDRUN_TEST_CREDENTIALS_FILE")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Cleanup(func() {
		t.Log("NOTE: clean up manually with: gcloud run services delete pipecd-conflict-test --project=" + project + " --region=" + region + " --quiet")
	})

	cl, err := client.NewClient(ctx, project, region, credentialsFile, zap.NewNop())
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	manifestYAML := `
apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: pipecd-conflict-test
spec:
  template:
    metadata:
      name: pipecd-conflict-test-v1
    spec:
      containers:
        - image: gcr.io/cloudrun/hello
`
	sm, err := client.ParseServiceManifest([]byte(manifestYAML))
	if err != nil {
		t.Fatalf("ParseServiceManifest failed: %v", err)
	}

	t.Log("First Create (should succeed)...")
	if _, err := cl.Create(ctx, sm); err != nil {
		t.Fatalf("first Create failed unexpectedly: %v", err)
	}

	t.Log("Second Create on the same service (expecting a conflict error)...")
	_, err = cl.Create(ctx, sm)
	if err == nil {
		t.Fatal("expected an error on the second Create, got nil")
	}
	t.Logf("Error type: %T", err)
	t.Logf("Error message: %v", err)
}
