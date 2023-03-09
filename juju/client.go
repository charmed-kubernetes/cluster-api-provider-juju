package juju

import (
	"context"
	"time"

	"github.com/juju/juju/api"
	"github.com/juju/juju/api/connector"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	PrefixUser          = "user-"
	connectionTimeout   = 10 * time.Minute
	UnspecifiedRevision = -1
)

type Configuration struct {
	ControllerAddresses []string
	Username            string
	Password            string
	CACert              string
}

type Client struct {
	Models       modelsClient
	Clouds       cloudsClient
	Credentials  credentialsClient
	Machines     machinesClient
	Controller   controllerClient
	Applications applicationsClient
	Integrations integrationsClient
	Users        usersClient
	Actions      actionsClient
}

type ConnectionFactory struct {
	config Configuration
}

func NewClient(config Configuration) (*Client, error) {
	cf := ConnectionFactory{
		config: config,
	}

	return &Client{
		Models:       *newModelsClient(cf),
		Clouds:       *newCloudsClient(cf),
		Credentials:  *newCredentialsClient(cf),
		Machines:     *newMachinesClient(cf),
		Controller:   *newControllerClient(cf),
		Applications: *newApplicationClient(cf),
		Integrations: *newIntegrationsClient(cf),
		Users:        *newUsersClient(cf),
		Actions:      *newActionsClient(cf),
	}, nil
}

func (cf *ConnectionFactory) GetConnection(ctx context.Context, modelUUID *string) (api.Connection, error) {
	// If modelUUID is nil, then we will end up with a controller connection
	// If its non-nil, we end up with a model connection
	log := log.FromContext(ctx)
	uuid := ""
	if modelUUID != nil {
		uuid = *modelUUID
	}

	dialOptions := func(do *api.DialOpts) {
		do.Timeout = connectionTimeout
		do.RetryDelay = 2 * time.Second
	}

	connr, err := connector.NewSimple(connector.SimpleConfig{
		ControllerAddresses: cf.config.ControllerAddresses,
		Username:            cf.config.Username,
		Password:            cf.config.Password,
		CACert:              cf.config.CACert,
		ModelUUID:           uuid,
	}, dialOptions)
	if err != nil {
		return nil, err
	}

	conn, err := connr.Connect()
	if err != nil {
		log.Error(err, "connection not established")
		return nil, err
	}
	return conn, nil
}
