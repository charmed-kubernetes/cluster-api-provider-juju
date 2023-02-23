package juju

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/juju/juju/api"
	"github.com/juju/juju/api/base"
	"github.com/juju/juju/api/client/modelconfig"
	"github.com/juju/juju/api/client/modelmanager"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/rpc/params"
	"github.com/juju/names/v4"
	"github.com/pkg/errors"
)

type modelsClient struct {
	ConnectionFactory
}

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

type DestroyModelInput struct {
	UUID string
}

func newModelsClient(cf ConnectionFactory) *modelsClient {
	return &modelsClient{
		ConnectionFactory: cf,
	}
}

func (c *modelsClient) getCurrentUser(conn api.Connection) string {
	return strings.TrimPrefix(conn.AuthTag().String(), PrefixUser)
}

func (c *modelsClient) resolveModelUUIDWithClient(client modelmanager.Client, name string, user string) (string, error) {
	modelUUID := ""
	modelSummaries, err := client.ListModelSummaries(user, false)
	if err != nil {
		return "", err
	}
	for _, modelSummary := range modelSummaries {
		if modelSummary.Name == name {
			modelUUID = modelSummary.UUID
			break
		}
	}

	if modelUUID == "" {
		return "", fmt.Errorf("model not found for defined user")
	}

	return modelUUID, nil
}

// GetModelUUID retrieves a model UUID by name
func (c *modelsClient) GetModelUUID(ctx context.Context, name string) (string, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return "", err
	}

	currentUser := c.getCurrentUser(conn)
	client := modelmanager.NewClient(conn)
	defer client.Close()

	modelUUID, err := c.resolveModelUUIDWithClient(*client, name, currentUser)
	if err != nil {
		return "", nil
	}

	return modelUUID, nil
}

// GetModelByName retrieves a model by name
func (c *modelsClient) GetModelByName(ctx context.Context, name string) (*params.ModelInfo, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return nil, err
	}

	currentUser := c.getCurrentUser(conn)
	client := modelmanager.NewClient(conn)
	defer client.Close()

	modelUUID, err := c.resolveModelUUIDWithClient(*client, name, currentUser)
	if err != nil {
		return nil, err
	}

	modelTag := names.NewModelTag(modelUUID)

	results, err := client.ModelInfo([]names.ModelTag{
		modelTag,
	})
	if err != nil {
		return nil, err
	}
	if len(results) < 1 {
		return nil, errors.Errorf("ModelInfo results were empty: %+v", results)
	}
	if len(results) > 1 {
		return nil, errors.Errorf("ModelInfo results contain results for more than one model: %+v", results)
	}

	if results[0].Error != nil {
		return nil, results[0].Error
	}

	modelInfo := results[0].Result

	return modelInfo, nil
}

func (c *modelsClient) ModelExists(ctx context.Context, name string) (bool, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return false, err
	}

	currentUser := strings.TrimPrefix(conn.AuthTag().String(), PrefixUser)

	client := modelmanager.NewClient(conn)
	defer client.Close()

	models, err := client.ListModels(currentUser)
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

func (c *modelsClient) CreateModel(ctx context.Context, input CreateModelInput) (*CreateModelResponse, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return nil, err
	}

	currentUser := strings.TrimPrefix(conn.AuthTag().String(), PrefixUser)

	client := modelmanager.NewClient(conn)
	defer client.Close()

	id := fmt.Sprintf("%s/%s/%s", input.Cloud, currentUser, input.CredentialName)
	if !names.IsValidCloudCredential(id) {
		return &CreateModelResponse{}, errors.Errorf("%q is not a valid credential id", id)
	}
	cloudCredTag := names.NewCloudCredentialTag(id)
	modelInfo, err := client.CreateModel(input.Name, currentUser, input.Cloud, input.CloudRegion, cloudCredTag, input.Config)
	if err != nil {
		return nil, err
	}

	// set constraints when required
	if input.Constraints.String() != "" {
		return &CreateModelResponse{ModelInfo: modelInfo}, nil
	}

	// we have to set constraints
	modelClient := modelconfig.NewClient(conn)
	defer modelClient.Close()
	err = modelClient.SetModelConstraints(input.Constraints)
	if err != nil {
		return nil, err
	}

	return &CreateModelResponse{ModelInfo: modelInfo}, nil
}

func (c *modelsClient) DestroyModel(ctx context.Context, input DestroyModelInput) error {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return err
	}

	client := modelmanager.NewClient(conn)
	defer client.Close()

	maxWait := 10 * time.Minute
	timeout := 30 * time.Minute

	tag := names.NewModelTag(input.UUID)

	destroyStorage := true
	forceDestroy := false

	err = client.DestroyModel(tag, &destroyStorage, &forceDestroy, &maxWait, timeout)
	if err != nil {
		return err
	}

	return nil
}
