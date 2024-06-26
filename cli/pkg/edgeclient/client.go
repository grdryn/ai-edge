/*
Copyright 2024. Open Data Hub Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package edgeclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kubeflow/model-registry/pkg/openapi"
	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	tektonclientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/opendatahub-io/ai-edge/cli/pkg/modelregistry"
)

// Client is a client representing the edge environment
//
// This client can be used to create and manage models and model container images suitable for deployment in edge environments.
type Client struct {
	modelRegistryClient *modelregistry.Client
}

// NewClient creates a new Client to interact with the edge environment. It requires the URL of the model registry service.
//
// This client can be used to create and manage models and model container images suitable for deployment in edge environments.
func NewClient(modelRegistryURL string) *Client {
	if !strings.Contains(modelRegistryURL, "://") {
		modelRegistryURL = "http://" + modelRegistryURL
	}
	return &Client{
		modelRegistryClient: modelregistry.NewClient(modelRegistryURL),
	}
}

// GetModels returns a list of models in the model registry.
func (c *Client) GetModels() ([]Model, error) {
	models, err := c.modelRegistryClient.GetRegisteredModels()
	if err != nil {
		return nil, fmt.Errorf("failed to get models: %w", err)
	}
	ms := make([]Model, len(models))
	for i, m := range models {
		ms[i] = Model{
			ID:          m.GetId(),
			Name:        m.GetName(),
			Description: m.GetDescription(),
		}
	}
	return ms, nil
}

// AddNewModelWithImage adds a model to the model registry along with the model version and the build parameters that
// will be used during the image build process.
func (c *Client) AddNewModelWithImage(
	modelName string,
	modelDescription string,
	modelVersion string,
	uri string,
	parameters map[string]interface{},
) (*ModelImage, error) {

	if modelName == "" || modelDescription == "" || modelVersion == "" {
		return nil, fmt.Errorf("model name, description, version, and URI are required")
	}

	modelFormatName := "ContainerImage"
	// This will be used to flag the model as edge compatible (i.e. has the required metadata to be built by the edge pipeline)
	parameters["edgeCompatible"] = "true"
	md, err := modelregistry.ToMetadataValueMap(parameters)
	if err != nil {
		return nil, fmt.Errorf("failed to add model image: %w", err)
	}
	m, v, a, err := c.modelRegistryClient.AutoRegisterModelVersionArtifact(
		modelName, modelDescription, modelVersion, modelName, uri, modelFormatName, "", md,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to add model image: %w", err)
	}

	return &ModelImage{
		ModelID:     m.GetId(),
		Name:        m.GetName(),
		Description: m.GetDescription(),
		Version:     v.GetName(),
		URI:         a.ModelArtifact.GetUri(),
	}, nil
}

// GetModelImages returns a list of model images in the model registry.
func (c *Client) GetModelImages() ([]ModelImage, error) {
	models, err := c.modelRegistryClient.GetRegisteredModels()
	if err != nil {
		return nil, fmt.Errorf("failed to get model images: %w", err)
	}
	images := make([]ModelImage, 0)
	for _, m := range models {
		versions, err := c.modelRegistryClient.GetModelVersions(m.GetId())
		if err != nil {
			if errors.Is(err, modelregistry.ErrModelNotFound) {
				return nil, fmt.Errorf("failed to get model images: can't find model with id %s", m.GetId())
			}
			return nil, fmt.Errorf("failed to get model images: %w", err)
		}
		for _, v := range versions {

			artifacts, err := c.modelRegistryClient.GetModelVersionArtifacts(v.GetId())
			if err != nil {
				if errors.Is(err, modelregistry.ErrVersionNotFound) {
					return nil, fmt.Errorf(
						"failed to get model images: can't find model version with id %s", v.GetId(),
					)
				}
				return nil, fmt.Errorf("failed to get model images: %w", err)
			}
			params, err := modelregistry.FromMetadataValueMap(v.GetCustomProperties())
			if err != nil {
				return nil, fmt.Errorf("failed to get model images: %w", err)
			}
			if len(artifacts) > 0 {

				// TODO: Set the status based on whether the image is built or not
				// TODO: Figure out where to show the image SHA
				// TODO: Figure out the URI
				// REF: https://issues.redhat.com/browse/RHOAIENG-6628
				for _, a := range artifacts {
					i := ModelImage{
						ModelID:     m.GetId(),
						Name:        m.GetName(),
						Description: m.GetDescription(),
						Version:     v.GetName(),
						BuildParams: params,
						URI:         a.ModelArtifact.GetUri(),
					}
					images = append(images, i)
				}
			} else {
				images = append(
					images, ModelImage{
						ModelID:     m.GetId(),
						Name:        m.GetName(),
						Description: m.GetDescription(),
						Version:     v.GetName(),
						BuildParams: params,
						URI:         "",
					},
				)
			}
		}
	}
	return images, nil
}

// UpdateModelImage synchronizes edge model image information with the model registry by ensuring that the model
// version and the model version artifact exist and are marked as edge compatible.
//
// If the model version or the model version artifact do not exist, they will be created.
// If the model version is not marked as edge compatible, it will be updated.
// If the model version custom properties do not match the provided parameters, they will be updated.
func (c *Client) UpdateModelImage(
	registeredModelID string,
	modelVersionName string,
	parameters map[string]interface{},
) (map[string]interface{}, error) {

	if registeredModelID == "" || modelVersionName == "" {
		return nil, fmt.Errorf("registered model ID and model version name required")
	}

	return c.ensureResourcesAreInModelRegistry(registeredModelID, modelVersionName, parameters)
}

// BuildModelImage builds a model container image for a model version identified by the registered model ID and model
// version name by creating a tekton PipelineRun in the specified namespace using the provided kubeconfig.
//
// For a tekton PipelineRun to build a model container image, the following must be true:
// 1. There is a registered model, a model version, and a model version artifact in the model registry.
// 2. The model version is marked as edge compatible (i.e. has the custom property "edgeCompatible" set to "true").
// 3. The model version custom properties include the build parameters required by the tekton PipelineRun.
//
// If parameters is nil, this method will try to use the custom properties of the model version if they exist.
//
// If parameters are provided they will be passed as parameters to the tekton PipelineRun.
func (c *Client) BuildModelImage(
	modelID string,
	modelVersion string,
	namespace string,
	kubeConfig string,
	parameters map[string]interface{},
) (*PipelineRun, error) {
	if modelID == "" || modelVersion == "" || namespace == "" || kubeConfig == "" {
		return nil, fmt.Errorf("model ID, model version, namespace, and kubeconfig are required")
	}
	m, err := c.modelRegistryClient.GetRegisteredModelByID(modelID)
	if err != nil {
		return nil, fmt.Errorf("failed to build model image: %w", err)
	}
	v, err := c.modelRegistryClient.FindModelVersion(modelID, modelVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to build model image: %w", err)
	}

	if parameters == nil {
		parameters, err = modelregistry.FromMetadataValueMap(v.GetCustomProperties())
		if err != nil {
			return nil, fmt.Errorf("failed to build model image: %w", err)
		}
	}

	return c.CreatePipelineRun(m.GetName(), modelVersion, namespace, kubeConfig, parameters)
}

// ensureResourcesAreInModelRegistry ensures that the model version and the model version artifact are in the model
// registry and returns the parameters to be used in the tekton PipelineRun. If the model version or the model version
// artifact do not exist, they will be created. If the parameters are nil, it will try to get the parameters from the
// model version custom properties.
func (c *Client) ensureResourcesAreInModelRegistry(
	registeredModelID string,
	modelVersionName string,
	parameters map[string]interface{},
) (map[string]interface{}, error) {
	model, err := c.modelRegistryClient.GetRegisteredModelByID(registeredModelID)
	if err != nil {
		if errors.Is(err, modelregistry.ErrModelNotFound) {
			return nil, fmt.Errorf("model not found. %w", err)
		}
		return nil, fmt.Errorf("failed to ensure resources are in model registry: %w", err)
	}
	v, parameters, err := c.ensureVersionIsInModelRegistry(registeredModelID, modelVersionName, parameters)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure resources are in model registry: %w", err)
	}
	err = c.ensureArtifactIsInModelRegistry(v.GetId(), model.GetName(), model.GetDescription(), "ContainerImage", "")
	if err != nil {
		return nil, fmt.Errorf("failed to ensure resources are in model registry: %w", err)
	}
	return parameters, nil
}

func (c *Client) ensureVersionIsInModelRegistry(
	registeredModelID string,
	modelVersionName string,
	parameters map[string]interface{},
) (*openapi.ModelVersion, map[string]interface{}, error) {
	v, err := c.modelRegistryClient.FindModelVersion(registeredModelID, modelVersionName)
	if err != nil {
		if errors.Is(err, modelregistry.ErrFindModelVersion) {
			if parameters == nil {
				// If the version is not found and no parameters are provided, we can't create the version
				return nil, nil, fmt.Errorf("model version not found and no parameters provided")
			}
			parameters["edgeCompatible"] = "true"
			md, err := modelregistry.ToMetadataValueMap(parameters)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to ensure version is in model registry: %w", err)
			}
			v, err = c.modelRegistryClient.CreateModelVersion(registeredModelID, modelVersionName, md)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to ensure version is in model registry: %w", err)
			}
		} else {
			return nil, nil, fmt.Errorf("failed to ensure version is in model registry: %w", err)
		}
	} else if parameters != nil {
		// If the version is found and parameters are provided, we update the version with the parameters
		parameters["edgeCompatible"] = "true"
		md, err := modelregistry.ToMetadataValueMap(parameters)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to ensure version is in model registry: %w", err)
		}
		v, err = c.modelRegistryClient.UpdateModelVersion(v.GetId(), md)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to ensure version is in model registry: %w", err)
		}
	}

	md, err := modelregistry.FromMetadataValueMap(v.GetCustomProperties())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to ensure version is in model registry: %w", err)
	}
	return v, md, nil
}

func (c *Client) ensureArtifactIsInModelRegistry(modelVersionID, artifactName, description, modelFormatName, modelFormatVersion string) error {
	artifact, err := c.modelRegistryClient.FindModelVersionArtifact(modelVersionID, artifactName)
	if err != nil {
		if errors.Is(err, modelregistry.ErrFindArtifact) {
			// TODO: Figure out what to do with the URI
			_, err := c.modelRegistryClient.CreateModelArtifact(
				modelVersionID, artifactName, description, "", modelFormatName, modelFormatVersion,
			)
			if err != nil {
				return fmt.Errorf("failed to ensure artifact is in model registry: %w", err)
			}
		} else {
			return fmt.Errorf("failed to ensure artifact is in model registry: %w", err)
		}
	} else if artifact == nil {
		// Should never happen as we get an error if the artifact is not found, but just in case
		return fmt.Errorf("failed to ensure artifact is in model registry: artifact not found")
	}

	return nil
}

// CreatePipelineRun creates a tekton PipelineRun to build a model container image from a model version artifact.
func (c *Client) CreatePipelineRun(
	modelName string,
	modelVersion string,
	namespace string,
	kubeConfig string,
	parameters map[string]interface{},
) (*PipelineRun, error) {
	var s3SecretName string
	var testDataConfigMapName string

	if s3s, ok := parameters["s3SecretName"]; !ok {
		return nil, fmt.Errorf("s3SecretName pipeline parameter is required")
	} else if s3s, ok := s3s.(string); !ok {
		return nil, fmt.Errorf("s3SecretName pipeline parameter must be a string")
	} else {
		s3SecretName = s3s
	}

	if tdc, ok := parameters["testDataConfigMapName"]; !ok {
		return nil, fmt.Errorf("testDataConfigMapName pipeline parameter is required")
	} else if tdc, ok := tdc.(string); !ok {
		return nil, fmt.Errorf("testDataConfigMapName pipeline parameter must be a string")
	} else {
		testDataConfigMapName = tdc
	}

	params, err := toTektonParams(modelName, modelVersion, parameters)
	if err != nil {
		return nil, fmt.Errorf("failed to convert parameters to tekton params: %w", err)
	}

	pipelineRun := newPipelineRunObject(
		modelName, namespace, params, s3SecretName, testDataConfigMapName,
	)

	config, _ := clientcmd.BuildConfigFromFlags("", kubeConfig)
	tektonClient, _ := tektonclientset.NewForConfig(config)

	createdPipelineRun, err := tektonClient.TektonV1().PipelineRuns(namespace).Create(
		context.Background(), pipelineRun, metav1.CreateOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create pipeline run: %w", err)
	}
	return &PipelineRun{
		Name:      createdPipelineRun.GetName(),
		Namespace: createdPipelineRun.GetObjectMeta().GetNamespace(),
	}, nil
}

func newPipelineRunObject(
	modelName string,
	namespace string,
	params tektonv1.Params,
	s3SecretName string,
	testDataConfigMapName string,
) *tektonv1.PipelineRun {
	pipelineRun := &tektonv1.PipelineRun{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    namespace,
			GenerateName: "aiedge-e2e-" + modelName + "-",
			Labels: map[string]string{
				"tekton.dev/pipeline": "aiedge-e2e",
				"model-name":          modelName,
			},
		},
		Spec: tektonv1.PipelineRunSpec{
			TaskRunTemplate: tektonv1.PipelineTaskRunTemplate{
				ServiceAccountName: "pipeline",
			},
			Params: params,
			PipelineRef: &tektonv1.PipelineRef{
				Name: "aiedge-e2e",
			},
			Workspaces: []tektonv1.WorkspaceBinding{
				{
					Name: "build-workspace-pv",
					VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
				{
					Name: "s3-secret",
					Secret: &corev1.SecretVolumeSource{
						SecretName: s3SecretName,
					},
				},
				{
					Name: "test-data",
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: testDataConfigMapName,
						},
					},
				},
			},
		},
	}
	return pipelineRun
}

func toTektonParams(modelName, modelVersion string, parameters map[string]interface{}) (tektonv1.Params, error) {
	params := tektonv1.Params{
		{
			Name:  "model-name",
			Value: *tektonv1.NewStructuredValues(modelName),
		},
		{
			Name:  "model-version",
			Value: *tektonv1.NewStructuredValues(modelVersion),
		},
	}
	for k, v := range parameters {
		var pv *tektonv1.ParamValue
		if s, ok := v.(string); ok {
			pv = tektonv1.NewStructuredValues(s)
		} else if sv, ok := v.([]interface{}); ok {
			pv = &tektonv1.ParamValue{Type: tektonv1.ParamTypeArray}
			for _, s := range sv {
				if ss, ok := s.(string); ok {
					pv.ArrayVal = append(pv.ArrayVal, ss)
				} else {
					return nil, fmt.Errorf("paramater %s has unsupported type %T", k, s)
				}
			}
		} else if sv, ok := v.([]string); ok {
			pv = &tektonv1.ParamValue{Type: tektonv1.ParamTypeArray}
			for _, s := range sv {
				pv.ArrayVal = append(pv.ArrayVal, s)
			}
		} else {
			return nil, fmt.Errorf("paramater %s has unsupported type %T", k, v)
		}

		params = append(
			params, tektonv1.Param{Name: k, Value: *pv},
		)
	}
	return params, nil
}
