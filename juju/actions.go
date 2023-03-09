package juju

import (
	"context"

	"github.com/juju/juju/api/client/action"
)

type actionsClient struct {
	ConnectionFactory
}

func newActionsClient(cf ConnectionFactory) *actionsClient {
	return &actionsClient{
		ConnectionFactory: cf,
	}
}

func (c *actionsClient) EnqueueOperation(ctx context.Context, actions []action.Action, modelUUID string) (action.EnqueuedActions, error) {
	conn, err := c.GetConnection(ctx, &modelUUID)
	if err != nil {
		return action.EnqueuedActions{}, err
	}
	client := action.NewClient(conn)

	return client.EnqueueOperation(actions)
}

func (c *actionsClient) GetOperation(ctx context.Context, operationID string, modelUUID string) (action.Operation, error) {
	conn, err := c.GetConnection(ctx, &modelUUID)
	if err != nil {
		return action.Operation{}, err
	}
	client := action.NewClient(conn)

	return client.Operation(operationID)
}
