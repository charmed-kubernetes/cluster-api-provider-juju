package juju

import (
	"context"
	"fmt"
	"strconv"

	"github.com/juju/charm/v9"
	charmresources "github.com/juju/charm/v9/resource"
	apiapplication "github.com/juju/juju/api/client/application"
	apicharms "github.com/juju/juju/api/client/charms"
	apiclient "github.com/juju/juju/api/client/client"
	apimodelconfig "github.com/juju/juju/api/client/modelconfig"
	apiresources "github.com/juju/juju/api/client/resources"
	"github.com/juju/juju/cmd/juju/application/utils"
	"github.com/juju/juju/core/constraints"
	coreseries "github.com/juju/juju/core/series"
	"github.com/juju/juju/rpc/params"
	"github.com/juju/names/v4"
	"github.com/pkg/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type applicationsClient struct {
	ConnectionFactory
}

type CreateApplicationInput struct {
	ApplicationName string
	ModelUUID       string
	CharmName       string
	CharmChannel    string
	CharmBase       string
	CharmRevision   int
	Units           int
	Trust           bool
	Expose          map[string]interface{}
	Config          map[string]interface{}
	Constraints     constraints.Value
}

type CreateApplicationResponse struct {
	AppName  string
	Revision int
	Base     string
}

type ReadApplicationInput struct {
	ModelUUID       string
	ApplicationName string
}

type ReadApplicationResponse struct {
	Name        string
	Channel     string
	Revision    int
	Base        params.Base
	Units       int
	Trust       bool
	Config      map[string]ConfigEntry
	Constraints constraints.Value
	Expose      map[string]interface{}
	Principal   bool
	Status      params.ApplicationStatus
}

type DestroyApplicationInput struct {
	ApplicationName string
	ModelUUID       string
}

type ApplicationExistsInput struct {
	ApplicationName string
	ModelUUID       string
}

// ConfigEntry is an auxiliar struct to
// keep information about juju config entries.
// Specially, we want to know if they have the
// default value.
type ConfigEntry struct {
	Value     interface{}
	IsDefault bool
}

// ConfigEntryToString returns the string representation based on
// the current value.
func ConfigEntryToString(input interface{}) string {
	switch t := input.(type) {
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', 0, 64)
	default:
		return input.(string)
	}
}

func newApplicationClient(cf ConnectionFactory) *applicationsClient {
	return &applicationsClient{
		ConnectionFactory: cf,
	}
}

func resolveCharmURL(charmName string) (*charm.URL, error) {
	path, err := charm.EnsureSchema(charmName, charm.CharmHub)
	if err != nil {
		return nil, err
	}
	charmURL, err := charm.ParseURL(path)
	if err != nil {
		return nil, err
	}

	return charmURL, nil
}

func (c applicationsClient) CreateApplication(ctx context.Context, input *CreateApplicationInput) (*CreateApplicationResponse, error) {
	log := log.FromContext(ctx)
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return nil, err
	}

	charmsAPIClient := apicharms.NewClient(conn)
	defer charmsAPIClient.Close()

	clientAPIClient := apiclient.NewClient(conn)
	defer clientAPIClient.Close()

	applicationAPIClient := apiapplication.NewClient(conn)
	defer applicationAPIClient.Close()

	modelconfigAPIClient := apimodelconfig.NewClient(conn)
	defer modelconfigAPIClient.Close()

	resourcesAPIClient, err := apiresources.NewClient(conn)
	if err != nil {
		return nil, err
	}

	defer resourcesAPIClient.Close()

	appName := input.ApplicationName
	if appName == "" {
		appName = input.CharmName
	}
	if err := names.ValidateApplicationName(appName); err != nil {
		return nil, err
	}

	channel, err := charm.ParseChannel(input.CharmChannel)
	if err != nil {
		return nil, err
	}

	charmURL, err := resolveCharmURL(input.CharmName)
	if err != nil {
		return nil, err
	}

	if charmURL.Revision != UnspecifiedRevision {
		return nil, fmt.Errorf("cannot specify revision in a charm or bundle name")
	}
	if input.CharmRevision != UnspecifiedRevision && channel.Empty() {
		return nil, fmt.Errorf("specifying a revision requires a channel for future upgrades")
	}

	modelConstraints, err := modelconfigAPIClient.GetModelConstraints()
	if err != nil {
		return nil, err
	}

	// Set base
	base, err := coreseries.ParseBaseFromString(input.CharmBase)
	if err != nil {
		return nil, err
	}

	series, err := coreseries.GetSeriesFromBase(base)
	if err != nil {
		return nil, err
	}

	platform, err := utils.DeducePlatform(constraints.Value{}, series, modelConstraints)
	if err != nil {
		return nil, err
	}

	urlForOrigin := charmURL
	if input.CharmRevision != UnspecifiedRevision {
		urlForOrigin = urlForOrigin.WithRevision(input.CharmRevision)
	}
	origin, err := utils.DeduceOrigin(urlForOrigin, channel, platform)
	if err != nil {
		return nil, err
	}
	// Charm or bundle has been supplied as a URL so we resolve and
	// deploy using the store but pass in the origin command line
	// argument so users can target a specific origin.
	resolved, err := charmsAPIClient.ResolveCharms([]apicharms.CharmToResolve{{URL: charmURL, Origin: origin}})
	if err != nil {
		return nil, err
	}
	if len(resolved) != 1 {
		return nil, fmt.Errorf("expected only one resolution, received %d", len(resolved))
	}
	resolvedCharm := resolved[0]

	if resolvedCharm.Error != nil {
		log.Info("Error when resolving charm", "charm", resolvedCharm)
		return nil, resolvedCharm.Error
	}

	// Add the charm to the model
	origin = resolvedCharm.Origin.WithBase(&base)

	var deployRevision int
	if input.CharmRevision > -1 {
		deployRevision = input.CharmRevision
	} else {
		if origin.Revision != nil {
			deployRevision = *origin.Revision
		} else {
			return nil, errors.New("no origin revision")
		}
	}

	charmURL = resolvedCharm.URL.WithRevision(deployRevision).WithArchitecture(origin.Architecture).WithSeries(series)
	resultOrigin, err := charmsAPIClient.AddCharm(charmURL, origin, false)
	if err != nil {
		return nil, err
	}

	charmID := apiapplication.CharmID{
		URL:    charmURL,
		Origin: resultOrigin,
	}

	resources, err := c.processResources(charmsAPIClient, resourcesAPIClient, charmID, input.ApplicationName)
	if err != nil {
		return nil, err
	}

	// The deploy API endpoint expects string values for the
	// constraints.
	var appConfig map[string]string
	if input.Config == nil {
		appConfig = make(map[string]string)
	} else {
		appConfig = make(map[string]string, len(input.Config))
		for k, v := range input.Config {
			appConfig[k] = ConfigEntryToString(v)
		}
	}

	appConfig["trust"] = fmt.Sprintf("%v", input.Trust)

	err = applicationAPIClient.Deploy(apiapplication.DeployArgs{
		CharmID:         charmID,
		ApplicationName: appName,
		NumUnits:        input.Units,
		CharmOrigin:     resultOrigin,
		Config:          appConfig,
		Cons:            input.Constraints,
		Resources:       resources,
	})

	if err != nil {
		// unfortunate error during deployment
		return &CreateApplicationResponse{
			AppName:  appName,
			Revision: *origin.Revision,
			Base:     origin.Base.String(),
		}, err
	}

	return &CreateApplicationResponse{
		AppName:  appName,
		Revision: *origin.Revision,
		Base:     origin.Base.String(),
	}, err
}

// processResources is a helper function to process the charm
// metadata and request the download of any additional resource.
func (c applicationsClient) processResources(charmsAPIClient *apicharms.Client, resourcesAPIClient *apiresources.Client, charmID apiapplication.CharmID, appName string) (map[string]string, error) {
	charmInfo, err := charmsAPIClient.CharmInfo(charmID.URL.String())
	if err != nil {
		return nil, err
	}

	// check if we have resources to request
	if len(charmInfo.Meta.Resources) == 0 {
		return nil, nil
	}

	pendingResources := []charmresources.Resource{}
	for _, v := range charmInfo.Meta.Resources {
		aux := charmresources.Resource{
			Meta: charmresources.Meta{
				Name:        v.Name,
				Type:        v.Type,
				Path:        v.Path,
				Description: v.Description,
			},
			Origin: charmresources.OriginStore,
			// TODO: prepare for resources with different versions
			Revision: -1,
		}
		pendingResources = append(pendingResources, aux)
	}

	resourcesReq := apiresources.AddPendingResourcesArgs{
		ApplicationID: appName,
		CharmID: apiresources.CharmID{
			URL:    charmID.URL,
			Origin: charmID.Origin,
		},
		CharmStoreMacaroon: nil,
		Resources:          pendingResources,
	}

	toRequest, err := resourcesAPIClient.AddPendingResources(resourcesReq)
	if err != nil {
		return nil, err
	}

	// now build a map with the resource name and the corresponding UUID
	toReturn := map[string]string{}
	for i, argsResource := range pendingResources {
		toReturn[argsResource.Meta.Name] = toRequest[i]
	}

	return toReturn, nil
}

func (c applicationsClient) ReadApplication(ctx context.Context, input *ReadApplicationInput) (*ReadApplicationResponse, error) {
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return nil, err
	}

	applicationAPIClient := apiapplication.NewClient(conn)
	defer applicationAPIClient.Close()

	charmsAPIClient := apicharms.NewClient(conn)
	defer charmsAPIClient.Close()

	clientAPIClient := apiclient.NewClient(conn)
	defer clientAPIClient.Close()

	apps, err := applicationAPIClient.ApplicationsInfo([]names.ApplicationTag{names.NewApplicationTag(input.ApplicationName)})
	if err != nil {
		return nil, err
	}
	if len(apps) > 1 {
		return nil, fmt.Errorf("more than one result for application: %s", input.ApplicationName)
	}
	if len(apps) < 1 {
		return nil, fmt.Errorf("no results for application: %s", input.ApplicationName)
	}
	appInfo := apps[0].Result

	var appConstraints constraints.Value = constraints.Value{}
	// constraints do not apply to subordinate applications.
	if appInfo.Principal {
		queryConstraints, err := applicationAPIClient.GetConstraints(input.ApplicationName)
		if err != nil {
			return nil, err
		}
		if len(queryConstraints) != 1 {
			return nil, fmt.Errorf("expected one set of application constraints, received %d", len(queryConstraints))
		}
		appConstraints = queryConstraints[0]
	}

	status, err := clientAPIClient.Status(nil)
	if err != nil {
		return nil, err
	}
	var appStatus params.ApplicationStatus
	var exists bool
	if appStatus, exists = status.Applications[input.ApplicationName]; !exists {
		return nil, fmt.Errorf("no status returned for application: %s", input.ApplicationName)
	}

	unitCount := len(appStatus.Units)

	// NOTE: we are assuming that this charm comes from CharmHub
	charmURL, err := charm.ParseURL(appStatus.Charm)
	if err != nil {
		return nil, fmt.Errorf("failed to parse charm: %v", err)
	}

	returnedConf, err := applicationAPIClient.Get("master", input.ApplicationName)
	if err != nil {
		return nil, fmt.Errorf("failed to get app configuration %v", err)
	}

	conf := make(map[string]ConfigEntry, 0)
	if returnedConf.ApplicationConfig != nil {
		for k, v := range returnedConf.ApplicationConfig {
			// skip the trust value. We have an independent field for that
			if k == "trust" {
				continue
			}
			// The API returns the configuration entries as interfaces
			aux := v.(map[string]interface{})
			// set if we find the value key and this is not a default
			// value.
			if value, found := aux["value"]; found {
				conf[k] = ConfigEntry{
					Value:     value,
					IsDefault: aux["source"] == "default",
				}
			}
		}
		// repeat the same steps for charm config values
		for k, v := range returnedConf.CharmConfig {
			aux := v.(map[string]interface{})
			if value, found := aux["value"]; found {
				conf[k] = ConfigEntry{
					Value:     value,
					IsDefault: aux["source"] == "default",
				}
			}
		}
	}

	// trust field which has to be included into the configuration
	trustValue := false
	if returnedConf.ApplicationConfig != nil {
		aux, found := returnedConf.ApplicationConfig["trust"]
		if found {
			m := aux.(map[string]any)
			target, found := m["value"]
			if found {
				trustValue = target.(bool)
			}
		}
	}

	response := &ReadApplicationResponse{
		Name:        charmURL.Name,
		Channel:     appStatus.CharmChannel,
		Revision:    charmURL.Revision,
		Base:        appInfo.Base,
		Units:       unitCount,
		Trust:       trustValue,
		Config:      conf,
		Constraints: appConstraints,
		Principal:   appInfo.Principal,
		Status:      appStatus,
	}

	return response, nil
}

func (c applicationsClient) DestroyApplication(ctx context.Context, input *DestroyApplicationInput) error {
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return err
	}

	applicationAPIClient := apiapplication.NewClient(conn)
	defer applicationAPIClient.Close()

	var destroyParams = apiapplication.DestroyApplicationsParams{
		Applications: []string{
			input.ApplicationName,
		},
		DestroyStorage: true,
	}

	_, err = applicationAPIClient.DestroyApplications(destroyParams)

	if err != nil {
		return err
	}

	return nil
}

func (c applicationsClient) ApplicationExists(ctx context.Context, input ApplicationExistsInput) (bool, error) {
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return false, err
	}

	applicationAPIClient := apiapplication.NewClient(conn)
	defer applicationAPIClient.Close()

	apps, err := applicationAPIClient.ApplicationsInfo([]names.ApplicationTag{names.NewApplicationTag(input.ApplicationName)})
	if err != nil {
		return false, err
	}
	if len(apps) < 1 {
		return false, nil
	}

	if apps[0].Error != nil {
		if apps[0].Error.Code == "not found" {
			return false, nil
		} else {
			return false, apps[0].Error
		}
	}

	return true, nil
}

func (c applicationsClient) AreApplicationUnitsActiveIdle(ctx context.Context, input ReadApplicationInput) (bool, error) {
	readAppResponse, err := c.ReadApplication(ctx, &input)
	if err != nil {
		return false, err
	}

	for _, unit := range readAppResponse.Status.Units {
		if unit.WorkloadStatus.Status != "active" || unit.AgentStatus.Status != "idle" {
			return false, nil
		}
	}

	return true, nil
}

func (c applicationsClient) GetLeaderAddress(ctx context.Context, input ReadApplicationInput) (*string, error) {
	readAppResponse, err := c.ReadApplication(ctx, &input)
	if err != nil {
		return nil, err
	}

	for _, unit := range readAppResponse.Status.Units {
		if unit.Leader {
			return &unit.PublicAddress, nil
		}
	}

	return nil, nil
}
