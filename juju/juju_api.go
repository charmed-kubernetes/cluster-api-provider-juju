package juju

import (
	"fmt"
	"strings"
	"time"

	"github.com/juju/juju/api"
	"github.com/juju/juju/api/base"
	"github.com/juju/juju/api/client/application"
	"github.com/juju/juju/api/client/cloud"
	"github.com/juju/juju/api/client/machinemanager"
	"github.com/juju/juju/api/client/modelconfig"
	"github.com/juju/juju/api/client/modelmanager"
	"github.com/juju/juju/api/client/usermanager"
	"github.com/juju/juju/api/connector"
	jujuCloud "github.com/juju/juju/cloud"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/rpc/params"
	"github.com/juju/names/v4"
	"github.com/pkg/errors"
)

type CreateModelInput struct {
	Name           string
	Cloud          string
	CloudRegion    string
	CredentialName string
	Config         map[string]interface{}
	Constraints    constraints.Value
}

type CreateModelResponse struct {
	ModelInfo base.ModelInfo
}

type JujuAPI struct {
	Connection           api.Connection
	applicationClient    *application.Client
	modelClient          *modelmanager.Client
	userClient           *usermanager.Client
	cloudClient          *cloud.Client
	modelConfigClient    *modelconfig.Client
	machineManagerClient *machinemanager.Client
}

func NewJujuAPi(connector *connector.SimpleConnector) (*JujuAPI, error) {
	conn, err := connector.Connect()
	if err != nil {
		return nil, fmt.Errorf("error connecting using Juju SimpleConnector: %v", err)
	}

	jujuAPI := &JujuAPI{}
	jujuAPI.Connection = conn
	jujuAPI.applicationClient = application.NewClient(conn)
	jujuAPI.modelClient = modelmanager.NewClient(conn)
	jujuAPI.cloudClient = cloud.NewClient(conn)
	jujuAPI.userClient = usermanager.NewClient(conn)
	jujuAPI.modelConfigClient = modelconfig.NewClient(conn)
	jujuAPI.machineManagerClient = machinemanager.NewClient(conn)
	return jujuAPI, nil
}

func (jujuAPI *JujuAPI) AddCloud(cloud jujuCloud.Cloud, force bool) error {
	return jujuAPI.cloudClient.AddCloud(cloud, force)
}

func (jujuAPI *JujuAPI) CloudExists(name string) (bool, error) {
	clouds, err := jujuAPI.cloudClient.Clouds()
	if err != nil {
		return false, err
	}

	for _, cloud := range clouds {
		if cloud.Name == name {
			return true, nil
		}
	}

	return false, nil
}

func (jujuAPI *JujuAPI) GetCurrentUser(conn api.Connection) string {
	return strings.TrimPrefix(conn.AuthTag().String(), "user-")
}

func (jujuAPI *JujuAPI) ModelExists(name string) (bool, error) {
	models, err := jujuAPI.modelClient.ListModels(jujuAPI.GetCurrentUser(jujuAPI.Connection))
	if err != nil {
		return false, err
	}

	for _, model := range models {
		if model.Name == name {
			return true, nil
		}
	}

	return false, nil
}

func (jujuAPI *JujuAPI) GetModelUUID(modelName string) (string, error) {
	modelUUID := ""
	user := jujuAPI.GetCurrentUser(jujuAPI.Connection)
	modelSummaries, err := jujuAPI.modelClient.ListModelSummaries(user, false)
	if err != nil {
		return "", err
	}
	for _, modelSummary := range modelSummaries {
		if modelSummary.Name == modelName {
			modelUUID = modelSummary.UUID
			break
		}
	}

	if modelUUID == "" {
		return "", fmt.Errorf("model not found for defined user")
	}

	return modelUUID, nil
}

func (jujuAPI *JujuAPI) GetModelInfo(modelName string) (params.ModelInfo, error) {
	modelUUID, err := jujuAPI.GetModelUUID(modelName)
	if err != nil {
		return params.ModelInfo{}, err
	}

	models, err := jujuAPI.modelClient.ModelInfo([]names.ModelTag{names.NewModelTag(modelUUID)})
	if err != nil {
		return params.ModelInfo{}, err
	}

	if len(models) > 1 {
		return params.ModelInfo{}, fmt.Errorf("more than one model returned for UUID: %s", modelUUID)
	}
	if len(models) < 1 {
		return params.ModelInfo{}, fmt.Errorf("no model returned for UUID: %s", modelUUID)
	}

	return *models[0].Result, nil
}

func (jujuAPI *JujuAPI) CreateModel(input CreateModelInput) (*CreateModelResponse, error) {
	currentUser := jujuAPI.GetCurrentUser(jujuAPI.Connection)
	id := fmt.Sprintf("%s/%s/%s", input.Cloud, currentUser, input.CredentialName)
	if !names.IsValidCloudCredential(id) {
		return &CreateModelResponse{}, errors.Errorf("%q is not a valid credential id", id)
	}
	cloudCredTag := names.NewCloudCredentialTag(id)
	modelInfo, err := jujuAPI.modelClient.CreateModel(input.Name, currentUser, input.Cloud, input.CloudRegion, cloudCredTag, input.Config)
	if err != nil {
		return nil, err
	}

	// set constraints when required
	if input.Constraints.String() != "" {
		return &CreateModelResponse{ModelInfo: modelInfo}, nil
	}

	// we have to set constraints
	err = jujuAPI.modelConfigClient.SetModelConstraints(input.Constraints)
	if err != nil {
		return nil, err
	}

	return &CreateModelResponse{ModelInfo: modelInfo}, nil
}

func (jujuAPI *JujuAPI) AddCredential(credential jujuCloud.Credential, credentialName string, cloudName string) error {
	user := jujuAPI.GetCurrentUser(jujuAPI.Connection)
	id := fmt.Sprintf("%s/%s/%s", cloudName, user, credentialName)
	if !names.IsValidCloudCredential(id) {
		return errors.Errorf("%q is not a valid credential id", id)
	}
	cloudCredTag := names.NewCloudCredentialTag(id)
	return jujuAPI.cloudClient.AddCredential(cloudCredTag.String(), credential)
}

func (jujuAPI *JujuAPI) CredentialExists(credentialName string, cloudName string) (bool, error) {
	user := jujuAPI.GetCurrentUser(jujuAPI.Connection)
	id := fmt.Sprintf("%s/%s/%s", cloudName, user, credentialName)
	if !names.IsValidCloudCredential(id) {
		return false, errors.Errorf("%q is not a valid credential id", id)
	}

	cloudCredTag := names.NewCloudCredentialTag(id)

	userCredTags, err := jujuAPI.cloudClient.UserCredentials(names.NewUserTag(user), names.NewCloudTag(cloudName))
	if err != nil {
		return false, err
	}

	for _, tag := range userCredTags {
		if tag == cloudCredTag {
			return true, nil
		}
	}

	return false, nil
}

func (jujuAPI *JujuAPI) AddMachine(machineParams params.AddMachineParams) (params.AddMachinesResult, error) {
	results, err := jujuAPI.machineManagerClient.AddMachines([]params.AddMachineParams{machineParams})
	if err != nil {
		return params.AddMachinesResult{}, err
	}

	return results[0], nil
}

func (jujuAPI *JujuAPI) DestroyMachine(force, keep, dryRun bool, maxWait *time.Duration, machineID string) (params.DestroyMachineResult, error) {
	results, err := jujuAPI.machineManagerClient.DestroyMachinesWithParams(force, keep, dryRun, maxWait, machineID)
	if err != nil {
		return params.DestroyMachineResult{}, err
	}

	return results[0], nil
}

func (jujuAPI *JujuAPI) MachineExistsInModel(machineID string, modelName string) (bool, error) {
	modelInfo, err := jujuAPI.GetModelInfo(modelName)
	if err != nil {
		return false, err
	}
	machines := modelInfo.Machines
	for _, machine := range machines {
		if machine.Id == machineID {
			return true, nil
		}
	}
	return false, nil
}
