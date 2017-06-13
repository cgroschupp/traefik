package openstack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/BurntSushi/ty/fun"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/pagination"
	"github.com/cenk/backoff"
	"github.com/containous/traefik/job"
	"github.com/containous/traefik/log"
	"github.com/containous/traefik/provider"
	"github.com/containous/traefik/safe"
	"github.com/containous/traefik/types"
)

var _ provider.Provider = (*Provider)(nil)

// Provider holds configurations of the provider.
type Provider struct {
	provider.BaseProvider `mapstructure:",squash"`

	Domain           string `description:"Default domain used"`
	ExposedByDefault bool   `description:"Expose containers by default"`
	RefreshSeconds   int    `description:"Polling interval (in seconds)"`

	// Provider lookup parameters
	Region		 string `description:"OpenStack region"`
	Username	 string `description:"OpenStack username"`
	Password	 string `description:"OpenStack password"`
	IdentityEndpoint string `description:"OpenStack endpoint"`
	DomainName	 string `description:"OpenStack domain name"`
	TenantName	 string `description:"OpenStack tenant name"`
}

type openstackInstance struct {
	Server           servers.Server
	Name		 string
	ID		 string
}

type openstackClient struct {
	serviceClient *gophercloud.ServiceClient
}

func (p *Provider) createClient() (*openstackClient, error) {
	opts := gophercloud.AuthOptions{
		IdentityEndpoint: p.IdentityEndpoint,
		Username:         p.Username,
		Password:         p.Password,
		TenantName:       p.TenantName,
		DomainName:       p.DomainName,
	}
	provider, err := openstack.AuthenticatedClient(opts)

	if err != nil {
		return nil, fmt.Errorf("could not create OpenStack session: %s", err)
	}

	client, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{
		Region: p.Region,
	})

	if err != nil {
		return nil, fmt.Errorf("could not create OpenStack compute session: %s", err)
	}

	return &openstackClient{
		client,
	}, nil
}

// Provide allows the OpenStack provider to provide configurations to traefik
// using the given configuration channel.
func (p *Provider) Provide(configurationChan chan<- types.ConfigMessage, pool *safe.Pool, constraints types.Constraints) error {

	p.Constraints = append(p.Constraints, constraints...)

	handleCanceled := func(ctx context.Context, err error) error {
		if ctx.Err() == context.Canceled || err == context.Canceled {
			return nil
		}
		return err
	}

	pool.Go(func(stop chan bool) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-stop:
				cancel()
			}
		}()

		operation := func() error {
			aws, err := p.createClient()
			if err != nil {
				return err
			}

			configuration, err := p.loadOpenstackConfig(ctx, aws)
			if err != nil {
				return handleCanceled(ctx, err)
			}

			configurationChan <- types.ConfigMessage{
				ProviderName:  "openstack",
				Configuration: configuration,
			}

			if p.Watch {
				reload := time.NewTicker(time.Second * time.Duration(p.RefreshSeconds))
				defer reload.Stop()
				for {
					select {
					case <-reload.C:
						configuration, err := p.loadOpenstackConfig(ctx, aws)
						if err != nil {
							return handleCanceled(ctx, err)
						}

						configurationChan <- types.ConfigMessage{
							ProviderName:  "openstack",
							Configuration: configuration,
						}
					case <-ctx.Done():
						return handleCanceled(ctx, ctx.Err())
					}
				}
			}

			return nil
		}

		notify := func(err error, time time.Duration) {
			log.Errorf("Provider connection error %+v, retrying in %s", err, time)
		}
		err := backoff.RetryNotify(safe.OperationWithRecover(operation), job.NewBackOff(backoff.NewExponentialBackOff()), notify)
		if err != nil {
			log.Errorf("Cannot connect to Provider api %+v", err)
		}
	})

	return nil
}

func (p *Provider) loadOpenstackConfig(ctx context.Context, client *openstackClient) (*types.Configuration, error) {
	var openstackFuncMap = template.FuncMap{
		"getFrontendRule": p.getFrontendRule,
		"filterFrontends": p.filterFrontends,
	}

	instances, err := p.listInstances(ctx, client)
	if err != nil {
		return nil, err
	}

	instances = fun.Filter(p.filterInstance, instances).([]openstackInstance)

	return p.GetConfiguration("templates/openstack.tmpl", openstackFuncMap, struct {
		Instances []openstackInstance
	}{
		instances,
	})
}

// Find all running Provider tasks in a cluster, also collect the task definitions (for docker labels)
// and the OpenStack instance data
func (p *Provider) listInstances(ctx context.Context, client *openstackClient) ([]openstackInstance, error) {
	opts := servers.ListOpts{}
	pager := servers.List(client.serviceClient, opts)

	var instances []openstackInstance
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		serverList, err := servers.ExtractServers(page)
		if err != nil {
			return false, fmt.Errorf("could not extract servers: %s", err)
		}

		for _, s := range serverList {
			instances = append(instances, openstackInstance{
				s,
				provider.Normalize(s.Name),
				s.ID,
			})
		}
		return true,nil
	})

	if err != nil {
		return nil, fmt.Errorf("error list servers")
	}

	return instances, nil
}

func (i openstackInstance) label(label string) (string, error) {
	if v, found := i.Server.Metadata[label]; found {
		return v,nil
	}
	return "", errors.New("Label not found:" + label)
}

func (i openstackInstance) privateIP() string {
	for _, address := range i.Server.Addresses {
		md, ok := address.([]interface{})
		if !ok {
			log.Warn("Invalid type for addresses, excepted array")
			continue
		}

		if len(md) == 0 {
			log.Debugf("Got no ip address for instance %s", i.Server.ID)
			continue
		}

		md1, ok := md[0].(map[string]interface{})
		if !ok {
			log.Warn("Invalid type for addresses, excepted dict")
			continue
		}

		addr, ok := md1["addr"].(string)
		if !ok {
			log.Warn("Invalid type for addresses, excepted string")
			continue
		}
		return addr
	}

	return ""
}

func (p *Provider) filterInstance(i openstackInstance) bool {
	if i.Server.Status != "ACTIVE" {
		log.Debugf("Filtering OpenStack instance in an incorrect state %s (%s) (state = %s)", i.Server.Name, i.Server.ID, i.Server.Status)
		return false
	}

	if i.privateIP() == "" {
		log.Debugf("Filtering OpenStack instance without an ip address %s (%s)", i.Server.Name, i.Server.ID)
		return false
	}

	label,_ := i.label("traefik.enable")
	enabled := p.ExposedByDefault && label != "false" || label == "true"
	if !enabled {
		log.Debugf("Filtering disabled OpenStack instance %s (%s) (traefik.enabled = '%s')", i.Server.Name, i.Server.ID, label)
		return false
	}

	return true
}

func (p *Provider) filterFrontends(instances []openstackInstance) []openstackInstance {
	byName := make(map[string]bool)

	return fun.Filter(func(i openstackInstance) bool {
		if _, found := byName[i.Backend()]; !found {
			byName[i.Backend()] = true
			return true
		}

		return false
	}, instances).([]openstackInstance)
}


func (p *Provider) getFrontendRule(i openstackInstance) string {
	if label,err := i.label("traefik.frontend.rule"); err == nil {
		return label
	}
	return "Host:" + strings.ToLower(strings.Replace(i.Name, "_", "-", -1)) + "." + p.Domain
}

func (i openstackInstance) Protocol() string {
	if label, err := i.label("traefik.protocol"); err == nil {
		return label
	}
	return "http"
}

func (i openstackInstance) Host() string {
	return i.privateIP()
}

func (i openstackInstance) Port() string {
	if label, err := i.label("traefik.port"); err == nil {
		return label
	}
	return "80"
}

func (i openstackInstance) Weight() string {
	if label, err := i.label("traefik.weight"); err == nil {
		return label
	}
	return "0"
}

func (i openstackInstance) PassHostHeader() string {
	if label, err := i.label("traefik.frontend.passHostHeader"); err == nil {
		return label
	}
	return "true"
}

func (i openstackInstance) Priority() string {
	if label, err := i.label("traefik.frontend.priority"); err == nil {
		return label
	}
	return "0"
}

func (i openstackInstance) Backend() string {
	if label, err := i.label("traefik.backend"); err == nil {
                return provider.Normalize(label)
	}
	return provider.Normalize(i.Name)
}

func (i openstackInstance) BalancerMethod() string {
	if label, err := i.label("traefik.backend.loadbalancer.method"); err == nil {
		return label
	}
	return "wrr"
}

func (i openstackInstance) EntryPoints() []string {
	if label, err := i.label("traefik.frontend.entryPoints"); err == nil {
		return strings.Split(label, ",")
	}
	return []string{"http", "https"}
}
