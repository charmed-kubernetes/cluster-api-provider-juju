package juju

import (
	"context"
	"fmt"
	"time"

	"github.com/juju/juju/api/client/machinemanager"
	"github.com/juju/juju/api/client/modelmanager"
	"github.com/juju/juju/rpc/params"
	"github.com/juju/names/v4"
	"github.com/pkg/errors"
)

type machinesClient struct {
	ConnectionFactory
}

type AddMachineInput struct {
	MachineParams params.AddMachineParams
	ModelUUID     string
}

type DestroyMachineInput struct {
	Force     bool
	Keep      bool
	DryRun    bool
	MaxWait   time.Duration
	MachineID string
	ModelUUID string
}

type GetMachineInput struct {
	ModelUUID string
	MachineID string
}

func newMachinesClient(cf ConnectionFactory) *machinesClient {
	return &machinesClient{
		ConnectionFactory: cf,
	}
}

func (c *machinesClient) AddMachine(ctx context.Context, input AddMachineInput) (params.AddMachinesResult, error) {
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return params.AddMachinesResult{}, err
	}

	client := machinemanager.NewClient(conn)
	defer client.Close()
	results, err := client.AddMachines([]params.AddMachineParams{input.MachineParams})
	if err != nil {
		return params.AddMachinesResult{}, err
	}
	if len(results) < 1 {
		return params.AddMachinesResult{}, errors.Errorf("AddMachines results were empty: %+v", results)
	}
	if len(results) > 1 {
		return params.AddMachinesResult{}, errors.Errorf("AddMachines results contain results for more than one machine: %+v", results)
	}

	if results[0].Error != nil {
		return params.AddMachinesResult{}, results[0].Error
	}

	return results[0], nil

}

func (c *machinesClient) DestroyMachine(ctx context.Context, input DestroyMachineInput) (params.DestroyMachineResult, error) {
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return params.DestroyMachineResult{}, err
	}

	client := machinemanager.NewClient(conn)
	defer client.Close()
	results, err := client.DestroyMachinesWithParams(input.Force, input.Keep, input.DryRun, &input.MaxWait, input.MachineID)
	if err != nil {
		return params.DestroyMachineResult{}, err
	}
	if len(results) < 1 {
		return params.DestroyMachineResult{}, errors.Errorf("DestroyMachines results were empty: %+v", results)
	}
	if len(results) > 1 {
		return params.DestroyMachineResult{}, errors.Errorf("DestroyMachines results contain results for more than one machine: %+v", results)
	}

	if results[0].Error != nil {
		return params.DestroyMachineResult{}, results[0].Error
	}

	return results[0], nil
}

func (c *machinesClient) GetMachine(ctx context.Context, input GetMachineInput) (*params.ModelMachineInfo, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return nil, err
	}

	client := modelmanager.NewClient(conn)
	defer client.Close()
	models, err := client.ModelInfo([]names.ModelTag{names.NewModelTag(input.ModelUUID)})
	if err != nil {
		return nil, err
	}

	if len(models) > 1 {
		return nil, fmt.Errorf("more than one model returned for UUID: %s", input.ModelUUID)
	}
	if len(models) < 1 {
		return nil, fmt.Errorf("no model returned for UUID: %s", input.ModelUUID)
	}

	model := models[0]
	if model.Result == nil {
		return nil, fmt.Errorf("model info result is nil for UUID: %s", input.ModelUUID)
	}

	modelInfo := *model.Result
	for _, machine := range modelInfo.Machines {
		if machine.Id == input.MachineID {
			return &machine, nil
		}
	}

	return nil, nil
}
