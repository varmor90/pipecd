package client

import (
	"fmt"
	"os"
	"strings"

	sdk "github.com/pipe-cd/piped-plugin-sdk-go"
	"google.golang.org/api/run/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// ServiceManifest wraps the raw service.yaml content as an unstructured
// object, so we can read and modify arbitrary fields before converting
// it to the strict run.Service type expected by the Cloud Run API.
type ServiceManifest struct {
	Name string
	u    *unstructured.Unstructured
}

// loadServiceManifest reads a service.yaml file from disk and parses it.
func loadServiceManifest(path string) (ServiceManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ServiceManifest{}, err
	}
	return ParseServiceManifest(data)
}

// LoadServiceManifest reads the service manifest file at the given path and parses it.
// This is the exported entry point other packages (e.g. deployment, livestate)
// should use to load a ServiceManifest from disk.
func LoadServiceManifest(path string) (ServiceManifest, error) {
	return loadServiceManifest(path)
}

// ParseServiceManifest parses the given YAML bytes into a ServiceManifest.
func ParseServiceManifest(data []byte) (ServiceManifest, error) {
	var obj unstructured.Unstructured
	if err := yaml.Unmarshal(data, &obj); err != nil {
		return ServiceManifest{}, err
	}

	return ServiceManifest{
		Name: obj.GetName(),
		u:    &obj,
	}, nil
}

// RunService converts the manifest into the run.Service type used by the
// Cloud Run API client.
func (m ServiceManifest) RunService() (*run.Service, error) {
	data, err := m.YamlBytes()
	if err != nil {
		return nil, err
	}

	var s run.Service
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// YamlBytes serializes the manifest back to YAML.
func (m ServiceManifest) YamlBytes() ([]byte, error) {
	return yaml.Marshal(m.u)
}

// SetRevision sets the name of the revision that will be created on deploy.
func (m ServiceManifest) SetRevision(name string) error {
	return unstructured.SetNestedField(m.u.Object, name, "spec", "template", "metadata", "name")
}

// RevisionTraffic describes how much traffic a single revision should receive.
type RevisionTraffic struct {
	RevisionName string `json:"revisionName"`
	Percent      int    `json:"percent"`
}

// UpdateTraffic replaces the traffic configuration of the service.
func (m ServiceManifest) UpdateTraffic(revisions []RevisionTraffic) error {
	items := []interface{}{}
	for i := range revisions {
		out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&revisions[i])
		if err != nil {
			return fmt.Errorf("unable to set traffic for object: %w", err)
		}
		items = append(items, out)
	}

	return unstructured.SetNestedSlice(m.u.Object, items, "spec", "traffic")
}

// UpdateAllTraffic routes 100% of traffic to the given revision. This is
// what a quick-sync deploy uses once the new revision is ready.
func (m ServiceManifest) UpdateAllTraffic(revision string) error {
	return m.UpdateTraffic([]RevisionTraffic{
		{
			RevisionName: revision,
			Percent:      100,
		},
	})
}

// Labels returns the service-level labels.
func (m ServiceManifest) Labels() map[string]string {
	return m.u.GetLabels()
}

// RevisionLabels returns the labels that will be attached to the revision
// created from this manifest.
func (m ServiceManifest) RevisionLabels() map[string]string {
	v, _, _ := unstructured.NestedStringMap(m.u.Object, "spec", "template", "metadata", "labels")
	return v
}

// AppID returns the PipeCD application ID stored in the service labels, if any.
func (m ServiceManifest) AppID() (string, bool) {
	v := m.Labels()
	if v == nil || v[LabelApplication] == "" {
		return "", false
	}
	return v[LabelApplication], true
}

// AddLabels merges the given labels into the existing service labels.
func (m ServiceManifest) AddLabels(labels map[string]string) {
	if len(labels) == 0 {
		return
	}

	lbls := m.u.GetLabels()
	if lbls == nil {
		m.u.SetLabels(labels)
		return
	}
	for k, v := range labels {
		lbls[k] = v
	}
	m.u.SetLabels(lbls)
}

// AddRevisionLabels merges the given labels into the revision template labels.
func (m ServiceManifest) AddRevisionLabels(labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}

	fields := []string{"spec", "template", "metadata", "labels"}
	lbls, ok, err := unstructured.NestedStringMap(m.u.Object, fields...)
	if err != nil {
		return err
	}
	if !ok {
		return unstructured.SetNestedStringMap(m.u.Object, labels, fields...)
	}

	for k, v := range labels {
		lbls[k] = v
	}
	return unstructured.SetNestedStringMap(m.u.Object, lbls, fields...)
}

// parseContainerImage splits a container image reference (e.g.
// "gcr.io/my-project/my-app:v2") into its name and tag.
func parseContainerImage(image string) (name, tag string) {
	parts := strings.Split(image, ":")
	if len(parts) == 2 {
		tag = parts[1]
	}
	paths := strings.Split(parts[0], "/")
	name = paths[len(paths)-1]
	return
}

// FindImageTag returns the tag of the first container's image in the manifest.
func FindImageTag(sm ServiceManifest) (string, error) {
	containers, ok, err := unstructured.NestedSlice(sm.u.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return "", err
	}
	if !ok || len(containers) == 0 {
		return "", fmt.Errorf("spec.template.spec.containers was missing")
	}

	container, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&containers[0])
	if err != nil {
		return "", fmt.Errorf("invalid container format")
	}

	image, ok, err := unstructured.NestedString(container, "image")
	if err != nil {
		return "", err
	}
	if !ok || image == "" {
		return "", fmt.Errorf("image was missing")
	}
	_, tag := parseContainerImage(image)

	return tag, nil
}

// FindArtifactVersions extracts the container image of the manifest and
// returns it as a single ArtifactVersion. This is what DetermineVersions
// reports back to the core as the target version.
func FindArtifactVersions(sm ServiceManifest) ([]sdk.ArtifactVersion, error) {
	containers, ok, err := unstructured.NestedSlice(sm.u.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return nil, err
	}
	if !ok || len(containers) == 0 {
		return nil, fmt.Errorf("spec.template.spec.containers was missing")
	}

	container, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&containers[0])
	if err != nil {
		return nil, fmt.Errorf("invalid container format")
	}

	image, ok, err := unstructured.NestedString(container, "image")
	if err != nil {
		return nil, err
	}
	if !ok || image == "" {
		return nil, fmt.Errorf("image was missing")
	}
	name, tag := parseContainerImage(image)

	return []sdk.ArtifactVersion{
		{
			Version: tag,
			Name:    name,
			URL:     image,
		},
	}, nil
}

// DecideRevisionName builds a unique revision name from the service name,
// the image tag, and a short commit hash, e.g. "my-app-v2-a1b2c3d".
// If the image has no tag (e.g. "gcr.io/cloudrun/hello" with no ":version"),
// that segment is omitted instead of leaving a stray "--" in the name.
func DecideRevisionName(sm ServiceManifest, commit string) (string, error) {
	tag, err := FindImageTag(sm)
	if err != nil {
		return "", err
	}
	tag = strings.ReplaceAll(tag, ".", "")

	if len(commit) > 7 {
		commit = commit[:7]
	}

	parts := []string{sm.Name}
	if tag != "" {
		parts = append(parts, tag)
	}
	if commit != "" {
		parts = append(parts, commit)
	}
	return strings.Join(parts, "-"), nil
}
