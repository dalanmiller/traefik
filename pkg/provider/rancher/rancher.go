package rancher

import (
	"context"
	"fmt"
	"text/template"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/containous/traefik/pkg/config"
	"github.com/containous/traefik/pkg/job"
	"github.com/containous/traefik/pkg/log"
	"github.com/containous/traefik/pkg/provider"
	"github.com/containous/traefik/pkg/safe"
	rancher "github.com/rancher/go-rancher-metadata/metadata"
)

const (
	// DefaultTemplateRule The default template for the default rule.
	DefaultTemplateRule = "Host(`{{ normalize .Name }}`)"
)

// Health
const (
	healthy         = "healthy"
	updatingHealthy = "updating-healthy"
)

// State
const (
	active          = "active"
	running         = "running"
	upgraded        = "upgraded"
	upgrading       = "upgrading"
	updatingActive  = "updating-active"
	updatingRunning = "updating-running"
)

var _ provider.Provider = (*Provider)(nil)

// Provider holds configurations of the provider.
type Provider struct {
	provider.Constrainer `description:"List of constraints used to filter out some containers." export:"true"`

	Watch                     bool   `description:"Watch provider." export:"true"`
	DefaultRule               string `description:"Default rule."`
	ExposedByDefault          bool   `description:"Expose containers by default." export:"true"`
	EnableServiceHealthFilter bool   `description:"Filter services with unhealthy states and inactive states." export:"true"`
	RefreshSeconds            int    `description:"Defines the polling interval in seconds." export:"true"`
	IntervalPoll              bool   `description:"Poll the Rancher metadata service every 'rancher.refreshseconds' (less accurate)."`
	Prefix                    string `description:"Prefix used for accessing the Rancher metadata service."`
	defaultRuleTpl            *template.Template
}

// SetDefaults sets the default values.
func (p *Provider) SetDefaults() {
	p.Watch = true
	p.ExposedByDefault = true
	p.EnableServiceHealthFilter = true
	p.RefreshSeconds = 15
	p.DefaultRule = DefaultTemplateRule
	p.Prefix = "latest"
}

type rancherData struct {
	Name       string
	Labels     map[string]string
	Containers []string
	Health     string
	State      string
	Port       string
	ExtraConf  configuration
}

// Init the provider.
func (p *Provider) Init() error {
	defaultRuleTpl, err := provider.MakeDefaultRuleTemplate(p.DefaultRule, nil)
	if err != nil {
		return fmt.Errorf("error while parsing default rule: %v", err)
	}

	p.defaultRuleTpl = defaultRuleTpl
	return nil
}

func (p *Provider) createClient(ctx context.Context) (rancher.Client, error) {
	metadataServiceURL := fmt.Sprintf("http://rancher-metadata.rancher.internal/%s", p.Prefix)
	client, err := rancher.NewClientAndWait(metadataServiceURL)
	if err != nil {
		log.FromContext(ctx).Errorf("Failed to create Rancher metadata service client: %v", err)
		return nil, err
	}

	return client, nil
}

// Provide allows the rancher provider to provide configurations to traefik using the given configuration channel.
func (p *Provider) Provide(configurationChan chan<- config.Message, pool *safe.Pool) error {
	pool.GoCtx(func(routineCtx context.Context) {
		ctxLog := log.With(routineCtx, log.Str(log.ProviderName, "rancher"))
		logger := log.FromContext(ctxLog)

		operation := func() error {
			client, err := p.createClient(ctxLog)
			if err != nil {
				logger.Errorf("Failed to create the metadata client metadata service: %v", err)
				return err
			}

			updateConfiguration := func(_ string) {
				stacks, err := client.GetStacks()
				if err != nil {
					logger.Errorf("Failed to query Rancher metadata service: %v", err)
					return
				}

				rancherData := p.parseMetadataSourcedRancherData(ctxLog, stacks)

				logger.Printf("Received Rancher data %+v", rancherData)

				configuration := p.buildConfiguration(ctxLog, rancherData)
				configurationChan <- config.Message{
					ProviderName:  "rancher",
					Configuration: configuration,
				}
			}
			updateConfiguration("init")

			if p.Watch {
				if p.IntervalPoll {
					p.intervalPoll(ctxLog, client, updateConfiguration)
				} else {
					// Long polling should be favored for the most accurate configuration updates.
					// Holds the connection until there is either a change in the metadata repository or `p.RefreshSeconds` has elapsed.
					client.OnChangeCtx(ctxLog, p.RefreshSeconds, updateConfiguration)
				}
			}

			return nil
		}

		notify := func(err error, time time.Duration) {
			logger.Errorf("Provider connection error %+v, retrying in %s", err, time)
		}
		err := backoff.RetryNotify(safe.OperationWithRecover(operation), backoff.WithContext(job.NewBackOff(backoff.NewExponentialBackOff()), ctxLog), notify)
		if err != nil {
			logger.Errorf("Cannot connect to Provider server: %+v", err)
		}
	})

	return nil
}

func (p *Provider) intervalPoll(ctx context.Context, client rancher.Client, updateConfiguration func(string)) {
	ticker := time.NewTicker(time.Second * time.Duration(p.RefreshSeconds))
	defer ticker.Stop()

	var version string
	for {
		select {
		case <-ticker.C:
			newVersion, err := client.GetVersion()
			if err != nil {
				log.FromContext(ctx).Errorf("Failed to create Rancher metadata service client: %v", err)
			} else if version != newVersion {
				version = newVersion
				updateConfiguration(version)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (p *Provider) parseMetadataSourcedRancherData(ctx context.Context, stacks []rancher.Stack) (rancherDataList []rancherData) {
	for _, stack := range stacks {
		for _, service := range stack.Services {
			ctxSvc := log.With(ctx, log.Str("stack", stack.Name), log.Str("service", service.Name))
			logger := log.FromContext(ctxSvc)

			servicePort := ""
			if len(service.Ports) > 0 {
				servicePort = service.Ports[0]
			}
			for _, port := range service.Ports {
				logger.Debugf("Set Port %s", port)
			}

			var containerIPAddresses []string
			for _, container := range service.Containers {
				if containerFilter(ctxSvc, container.Name, container.HealthState, container.State) {
					containerIPAddresses = append(containerIPAddresses, container.PrimaryIp)
				}
			}

			service := rancherData{
				Name:       service.Name + "/" + stack.Name,
				State:      service.State,
				Labels:     service.Labels,
				Port:       servicePort,
				Containers: containerIPAddresses,
			}

			extraConf, err := p.getConfiguration(service)
			if err != nil {
				logger.Errorf("Skip container %s: %v", service.Name, err)
				continue
			}

			service.ExtraConf = extraConf

			rancherDataList = append(rancherDataList, service)
		}
	}
	return rancherDataList
}

func containerFilter(ctx context.Context, name, healthState, state string) bool {
	logger := log.FromContext(ctx)

	if healthState != "" && healthState != healthy && healthState != updatingHealthy {
		logger.Debugf("Filtering container %s with healthState of %s", name, healthState)
		return false
	}

	if state != "" && state != running && state != updatingRunning && state != upgraded {
		logger.Debugf("Filtering container %s with state of %s", name, state)
		return false
	}

	return true
}
