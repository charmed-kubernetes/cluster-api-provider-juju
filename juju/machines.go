package juju

import (
	"context"
	"time"

	"github.com/juju/juju/api/client/machinemanager"
	"github.com/juju/juju/rpc/params"
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
