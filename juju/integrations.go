package juju

import (
	"context"
	"fmt"
	"strings"

	"github.com/juju/juju/api"
	apiapplication "github.com/juju/juju/api/client/application"
	apiclient "github.com/juju/juju/api/client/client"
	"github.com/juju/juju/rpc/params"
)

type integrationsClient struct {
	ConnectionFactory
}

type Application struct {
	Name     string
	Endpoint string
	Role     string
	OfferURL *string
}

type IntegrationInput struct {
	ModelUUID string
	Endpoints []string
	ViaCIDRs  string
}

type CreateIntegrationResponse struct {
	Applications []Application
}

func splitCommaDelimitedList(list string) []string {
	items := make([]string, 0)
	for _, token := range strings.Split(list, ",") {
		token = strings.TrimSpace(token)
		if len(token) == 0 {
			continue
		}
		items = append(items, token)
	}
	return items
}

func newIntegrationsClient(cf ConnectionFactory) *integrationsClient {
	return &integrationsClient{
		ConnectionFactory: cf,
	}
}

func (c integrationsClient) CreateIntegration(ctx context.Context, input *IntegrationInput) (*CreateIntegrationResponse, error) {
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return nil, err
	}

	client := apiapplication.NewClient(conn)
	defer client.Close()

	listViaCIDRs := splitCommaDelimitedList(input.ViaCIDRs)
	response, err := client.AddRelation(
		input.Endpoints,
		listViaCIDRs,
	)
	if err != nil {
		return nil, err
	}

	//integration is created - fetch the status in order to validate
	status, err := getStatus(conn)
	if err != nil {
		return nil, err
	}

	applications := parseApplications(status.RemoteApplications, response.Endpoints)

	return &CreateIntegrationResponse{
		Applications: applications,
	}, nil
}

func (c integrationsClient) IntegrationExists(ctx context.Context, input *IntegrationInput) (bool, error) {
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return false, err
	}

	status, err := getStatus(conn)
	if err != nil {
		return false, err
	}

	integrations := status.Relations
	var integration params.RelationStatus
	if len(integrations) == 0 {
		return false, nil
	}

	apps := make([][]string, 0, len(input.Endpoints))
	for _, v := range input.Endpoints {
		app := strings.Split(v, ":")
		apps = append(apps, []string{
			app[0],
			app[1],
		})
	}
	// the key is built assuming that the ID is "<provider>:<endpoint> <requirer>:<endpoint>"
	// the integrations that come back from status have the key formatted as "<requirer>:<endpoint> <provider>:<endpoint>"
	key := fmt.Sprintf("%v:%v %v:%v", apps[1][0], apps[1][1], apps[0][0], apps[0][1])

	for _, v := range integrations {
		if v.Key == key {
			integration = v
			break
		}
	}

	if integration.Id == 0 && integration.Key == "" {
		keyReversed := fmt.Sprintf("%v:%v %v:%v", apps[0][0], apps[0][1], apps[1][0], apps[1][1])

		for _, v := range integrations {
			if v.Key == keyReversed {
				integration = v
				break
			}
		}
	}

	if integration.Id == 0 && integration.Key == "" {
		return false, nil
	}

	return true, nil
}

func getStatus(conn api.Connection) (*params.FullStatus, error) {
	client := apiclient.NewClient(conn)
	defer client.Close()

	status, err := client.Status(nil)
	if err != nil {
		return nil, err
	}
	return status, nil
}

// This function takes remote applications and endpoint status and combines them into a more usable format to return to the provider
func parseApplications(remoteApplications map[string]params.RemoteApplicationStatus, src interface{}) []Application {
	applications := make([]Application, 0, 2)

	switch endpoints := src.(type) {
	case []params.EndpointStatus:
		if len(remoteApplications) != 0 {
			for index, endpoint := range endpoints {
				if remote, exists := remoteApplications[endpoint.ApplicationName]; exists {
					a := Application{
						Name:     endpoint.ApplicationName,
						Endpoint: endpoint.Name,
						Role:     endpoint.Role,
						OfferURL: &remote.OfferURL,
					}
					applications = append(applications, a)

					endpoints[index] = endpoints[len(endpoints)-1]
					endpoints = endpoints[:len(endpoints)-1]
				}
			}
		}
		for _, endpoint := range endpoints {
			a := Application{
				Name:     endpoint.ApplicationName,
				Endpoint: endpoint.Name,
				Role:     endpoint.Role,
				OfferURL: nil,
			}
			applications = append(applications, a)
		}
	case map[string]params.CharmRelation:
		if len(remoteApplications) != 0 {
			for index, endpoint := range endpoints {
				if remote, exists := remoteApplications[index]; exists {
					a := Application{
						Name:     index,
						Endpoint: endpoint.Name,
						Role:     endpoint.Role,
						OfferURL: &remote.OfferURL,
					}
					applications = append(applications, a)

					delete(endpoints, index)
				}
			}
		}
		for index, endpoint := range endpoints {
			a := Application{
				Name:     index,
				Endpoint: endpoint.Name,
				Role:     endpoint.Role,
				OfferURL: nil,
			}
			applications = append(applications, a)
		}
	}

	return applications
}
