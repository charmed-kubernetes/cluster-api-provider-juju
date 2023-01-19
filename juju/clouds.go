package juju

import (
	"context"

	"github.com/juju/juju/api/client/cloud"
	jujuCloud "github.com/juju/juju/cloud"
)

type cloudsClient struct {
	ConnectionFactory
}

type AddCloudInput struct {
	Cloud jujuCloud.Cloud
	Force bool
}

type CloudExistsInput struct {
	Name string
}

func newCloudsClient(cf ConnectionFactory) *cloudsClient {
	return &cloudsClient{
		ConnectionFactory: cf,
	}
}

func (c *cloudsClient) AddCloud(ctx context.Context, input AddCloudInput) error {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return err
	}

	client := cloud.NewClient(conn)
	defer client.Close()

	err = client.AddCloud(input.Cloud, input.Force)
	if err != nil {
		return err
	}

	return nil
}

func (c *cloudsClient) CloudExists(ctx context.Context, input CloudExistsInput) (bool, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return false, err
	}

	client := cloud.NewClient(conn)
	defer client.Close()

	clouds, err := client.Clouds()
	if err != nil {
		return false, err
	}

	for _, cloud := range clouds {
		if cloud.Name == input.Name {
			return true, nil
		}
	}

	return false, nil
}
