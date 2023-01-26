package juju

import (
	"context"
	"time"

	"github.com/juju/juju/api/controller/controller"
)

type controllerClient struct {
	ConnectionFactory
}

type DestroyControllerInput struct {
	DestroyModels  bool
	DestroyStorage bool
	Force          bool
	MaxWait        time.Duration `json:"max-wait,omitempty"`
	ModelTimeout   time.Duration
}

func newControllerClient(cf ConnectionFactory) *controllerClient {
	return &controllerClient{
		ConnectionFactory: cf,
	}
}

func (c *controllerClient) DestroyController(ctx context.Context, input DestroyControllerInput) error {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return err
	}

	destroyParams := controller.DestroyControllerParams{
		DestroyModels:  input.DestroyModels,
		DestroyStorage: &input.DestroyStorage,
		Force:          &input.Force,
		MaxWait:        &input.MaxWait,
		ModelTimeout:   &input.ModelTimeout,
	}

	client := controller.NewClient(conn)
	defer client.Close()
	return client.DestroyController(destroyParams)
}
