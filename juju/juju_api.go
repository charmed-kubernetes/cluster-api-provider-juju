package juju

import (
	"fmt"
	"strings"

	"github.com/juju/juju/api"
	"github.com/juju/juju/api/client/application"
	"github.com/juju/juju/api/client/cloud"
	"github.com/juju/juju/api/client/modelmanager"
	"github.com/juju/juju/api/client/usermanager"
	"github.com/juju/juju/api/connector"
	jujuCloud "github.com/juju/juju/cloud"
	"github.com/juju/names/v4"
	"github.com/pkg/errors"
)

type JujuAPI struct {
	Connection        api.Connection
	applicationClient *application.Client
	modelClient       *modelmanager.Client
	userClient        *usermanager.Client
	cloudClient       *cloud.Client
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
