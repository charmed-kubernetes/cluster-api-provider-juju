package juju

import (
	"context"
	"fmt"

	"github.com/juju/juju/api/client/cloud"
	jujuCloud "github.com/juju/juju/cloud"
	"github.com/juju/names/v4"
	"github.com/pkg/errors"
)

type credentialsClient struct {
	ConnectionFactory
}

type AddCredentialInput struct {
	Credential     jujuCloud.Credential
	CredentialName string
	CloudName      string
	UserName       string
}

type CredentialExistsInput struct {
	CredentialName string
	CloudName      string
	UserName       string
}

func newCredentialsClient(cf ConnectionFactory) *credentialsClient {
	return &credentialsClient{
		ConnectionFactory: cf,
	}
}

func (c *credentialsClient) AddCredential(ctx context.Context, input AddCredentialInput) error {

	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return err
	}

	client := cloud.NewClient(conn)
	defer client.Close()
	id := fmt.Sprintf("%s/%s/%s", input.CloudName, input.UserName, input.CredentialName)
	if !names.IsValidCloudCredential(id) {
		return errors.Errorf("%q is not a valid credential id", id)
	}
	cloudCredTag := names.NewCloudCredentialTag(id)
	return client.AddCredential(cloudCredTag.String(), input.Credential)
}

func (c *credentialsClient) CredentialExists(ctx context.Context, input CredentialExistsInput) (bool, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return false, err
	}

	id := fmt.Sprintf("%s/%s/%s", input.CloudName, input.UserName, input.CredentialName)
	if !names.IsValidCloudCredential(id) {
		return false, errors.Errorf("%q is not a valid credential id", id)
	}

	cloudCredTag := names.NewCloudCredentialTag(id)

	client := cloud.NewClient(conn)
	defer client.Close()

	userCredTags, err := client.UserCredentials(names.NewUserTag(input.UserName), names.NewCloudTag(input.CloudName))
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
