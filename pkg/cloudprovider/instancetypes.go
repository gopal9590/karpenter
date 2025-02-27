/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudprovider

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"

	awssettings "github.com/aws/karpenter/pkg/apis/settings"
	awscache "github.com/aws/karpenter/pkg/cache"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/logging"

	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/providers/subnet"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/utils/pretty"
)

const (
	InstanceTypesCacheKey           = "types"
	InstanceTypeZonesCacheKeyPrefix = "zones:"
	InstanceTypesAndZonesCacheTTL   = 5 * time.Minute
)

type InstanceTypeProvider struct {
	region          string
	ec2api          ec2iface.EC2API
	subnetProvider  *subnet.Provider
	pricingProvider *PricingProvider
	// Has one cache entry for all the instance types (key: InstanceTypesCacheKey)
	// Has one cache entry for all the zones for each subnet selector (key: InstanceTypesZonesCacheKeyPrefix:<hash_of_selector>)
	// Values cached *before* considering insufficient capacity errors from the unavailableOfferings cache.
	// Fully initialized Instance Types are also cached based on the set of all instance types, zones, unavailableOfferings cache,
	// node template, and kubelet configuration from the provisioner
	cache                *cache.Cache
	unavailableOfferings *awscache.UnavailableOfferings
	cm                   *pretty.ChangeMonitor
	// instanceTypesSeqNum is a monotonically increasing change counter used to avoid the expensive hashing operation on instance types
	instanceTypesSeqNum uint64
}

func NewInstanceTypeProvider(ctx context.Context, sess *session.Session, ec2api ec2iface.EC2API, subnetProvider *subnet.Provider,
	unavailableOfferingsCache *awscache.UnavailableOfferings, startAsync <-chan struct{}) *InstanceTypeProvider {
	return &InstanceTypeProvider{
		ec2api:         ec2api,
		region:         *sess.Config.Region,
		subnetProvider: subnetProvider,
		pricingProvider: NewPricingProvider(
			ctx,
			NewPricingAPI(sess, *sess.Config.Region),
			ec2api,
			*sess.Config.Region,
			awssettings.FromContext(ctx).IsolatedVPC,
			startAsync,
		),
		cache:                cache.New(InstanceTypesAndZonesCacheTTL, awscache.CleanupInterval),
		unavailableOfferings: unavailableOfferingsCache,
		cm:                   pretty.NewChangeMonitor(),
		instanceTypesSeqNum:  0,
	}
}

func (p *InstanceTypeProvider) List(ctx context.Context, kc *v1alpha5.KubeletConfiguration, nodeTemplate *v1alpha1.AWSNodeTemplate) ([]*cloudprovider.InstanceType, error) {
	// Get InstanceTypes from EC2
	instanceTypes, err := p.getInstanceTypes(ctx)
	if err != nil {
		return nil, err
	}
	// Get Viable EC2 Purchase offerings
	instanceTypeZones, err := p.getInstanceTypeZones(ctx, nodeTemplate)
	if err != nil {
		return nil, err
	}

	// Compute fully initialized instance types hash key
	instanceTypeZonesHash, _ := hashstructure.Hash(instanceTypeZones, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	kcHash, _ := hashstructure.Hash(kc, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	key := fmt.Sprintf("%d-%d-%s-%016x-%016x", p.instanceTypesSeqNum, p.unavailableOfferings.SeqNum, nodeTemplate.UID, instanceTypeZonesHash, kcHash)

	if item, ok := p.cache.Get(key); ok {
		return item.([]*cloudprovider.InstanceType), nil
	}

	var result []*cloudprovider.InstanceType
	for _, i := range instanceTypes {
		instanceTypeName := aws.StringValue(i.InstanceType)
		instanceType := NewInstanceType(ctx, i, kc, p.region, nodeTemplate, p.createOfferings(ctx, i, instanceTypeZones[instanceTypeName]))
		result = append(result, instanceType)
	}
	p.cache.SetDefault(key, result)
	return result, nil
}

func (p *InstanceTypeProvider) LivenessProbe(req *http.Request) error {
	if err := p.subnetProvider.LivenessProbe(req); err != nil {
		return err
	}
	if err := p.pricingProvider.LivenessProbe(req); err != nil {
		return err
	}
	return nil
}

func (p *InstanceTypeProvider) createOfferings(ctx context.Context, instanceType *ec2.InstanceTypeInfo, zones sets.String) []cloudprovider.Offering {
	var offerings []cloudprovider.Offering
	for zone := range zones {
		// while usage classes should be a distinct set, there's no guarantee of that
		for capacityType := range sets.NewString(aws.StringValueSlice(instanceType.SupportedUsageClasses)...) {
			// exclude any offerings that have recently seen an insufficient capacity error from EC2
			isUnavailable := p.unavailableOfferings.IsUnavailable(*instanceType.InstanceType, zone, capacityType)
			var price float64
			var ok bool
			switch capacityType {
			case ec2.UsageClassTypeSpot:
				price, ok = p.pricingProvider.SpotPrice(*instanceType.InstanceType, zone)
			case ec2.UsageClassTypeOnDemand:
				price, ok = p.pricingProvider.OnDemandPrice(*instanceType.InstanceType)
			default:
				logging.FromContext(ctx).Errorf("Received unknown capacity type %s for instance type %s", capacityType, *instanceType.InstanceType)
				continue
			}
			available := !isUnavailable && ok
			offerings = append(offerings, cloudprovider.Offering{
				Zone:         zone,
				CapacityType: capacityType,
				Price:        price,
				Available:    available,
			})
		}
	}
	return offerings
}

func (p *InstanceTypeProvider) getInstanceTypeZones(ctx context.Context, nodeTemplate *v1alpha1.AWSNodeTemplate) (map[string]sets.String, error) {
	subnetSelectorHash, err := hashstructure.Hash(nodeTemplate.Spec.SubnetSelector, hashstructure.FormatV2, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to hash the subnet selector: %w", err)
	}
	cacheKey := fmt.Sprintf("%s%016x", InstanceTypeZonesCacheKeyPrefix, subnetSelectorHash)
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.(map[string]sets.String), nil
	}

	// Constrain AZs from subnets
	subnets, err := p.subnetProvider.List(ctx, nodeTemplate)
	if err != nil {
		return nil, err
	}
	if len(subnets) == 0 {
		return nil, fmt.Errorf("no subnets matched selector %v", nodeTemplate.Spec.SubnetSelector)
	}
	zones := sets.NewString(lo.Map(subnets, func(subnet *ec2.Subnet, _ int) string {
		return aws.StringValue(subnet.AvailabilityZone)
	})...)

	// Get offerings from EC2
	instanceTypeZones := map[string]sets.String{}
	if err := p.ec2api.DescribeInstanceTypeOfferingsPagesWithContext(ctx, &ec2.DescribeInstanceTypeOfferingsInput{LocationType: aws.String("availability-zone")},
		func(output *ec2.DescribeInstanceTypeOfferingsOutput, lastPage bool) bool {
			for _, offering := range output.InstanceTypeOfferings {
				if zones.Has(aws.StringValue(offering.Location)) {
					if _, ok := instanceTypeZones[aws.StringValue(offering.InstanceType)]; !ok {
						instanceTypeZones[aws.StringValue(offering.InstanceType)] = sets.NewString()
					}
					instanceTypeZones[aws.StringValue(offering.InstanceType)].Insert(aws.StringValue(offering.Location))
				}
			}
			return true
		}); err != nil {
		return nil, fmt.Errorf("describing instance type zone offerings, %w", err)
	}
	if p.cm.HasChanged("zonal-offerings", nodeTemplate.Spec.SubnetSelector) {
		logging.FromContext(ctx).With("subnet-selector", pretty.Concise(nodeTemplate.Spec.SubnetSelector)).Debugf("discovered EC2 instance types zonal offerings for subnets")
	}
	p.cache.SetDefault(cacheKey, instanceTypeZones)
	return instanceTypeZones, nil
}

// getInstanceTypes retrieves all instance types from the ec2 DescribeInstanceTypes API using some opinionated filters
func (p *InstanceTypeProvider) getInstanceTypes(ctx context.Context) (map[string]*ec2.InstanceTypeInfo, error) {
	if cached, ok := p.cache.Get(InstanceTypesCacheKey); ok {
		return cached.(map[string]*ec2.InstanceTypeInfo), nil
	}
	instanceTypes := map[string]*ec2.InstanceTypeInfo{}
	if err := p.ec2api.DescribeInstanceTypesPagesWithContext(ctx, &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("supported-virtualization-type"),
				Values: []*string{aws.String("hvm")},
			},
			{
				Name:   aws.String("processor-info.supported-architecture"),
				Values: aws.StringSlice([]string{"x86_64", "arm64"}),
			},
		},
	}, func(page *ec2.DescribeInstanceTypesOutput, lastPage bool) bool {
		for _, instanceType := range page.InstanceTypes {
			instanceTypes[aws.StringValue(instanceType.InstanceType)] = instanceType
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("fetching instance types using ec2.DescribeInstanceTypes, %w", err)
	}
	if p.cm.HasChanged("instance-types", instanceTypes) {
		logging.FromContext(ctx).With(
			"instance-type-count", len(instanceTypes)).Debugf("discovered EC2 instance types")
	}
	atomic.AddUint64(&p.instanceTypesSeqNum, 1)
	p.cache.SetDefault(InstanceTypesCacheKey, instanceTypes)
	return instanceTypes, nil
}
