/*
Copyright 2019 The Kubernetes Authors.

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

package awsmodel

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/featureflag"
	"k8s.io/kops/pkg/model"
	"k8s.io/kops/pkg/model/defaults"
	"k8s.io/kops/pkg/model/spotinstmodel"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awstasks"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
)

const (
	// DefaultVolumeType is the default volume type
	DefaultVolumeType = ec2.VolumeTypeGp2
	// DefaultVolumeIonIops is the default volume IOPS when volume type is io1 or io2
	DefaultVolumeIonIops = 100
	// DefaultVolumeGp3Iops is the default volume IOPS when volume type is gp3
	DefaultVolumeGp3Iops = 3000
	// DefaultVolumeGp3Throughput is the default volume throughput when volume type is gp3
	DefaultVolumeGp3Throughput = 125
	// DefaultVolumeDeleteOnTermination is the default volume behavior after instance termination
	DefaultVolumeDeleteOnTermination = true
	// DefaultVolumeEncryption is the default volume encryption behavior
	DefaultVolumeEncryption = false
)

// AutoscalingGroupModelBuilder configures AutoscalingGroup objects
type AutoscalingGroupModelBuilder struct {
	*AWSModelContext

	BootstrapScriptBuilder *model.BootstrapScriptBuilder
	Lifecycle              *fi.Lifecycle
	SecurityLifecycle      *fi.Lifecycle
}

var _ fi.ModelBuilder = &AutoscalingGroupModelBuilder{}

// Build is responsible for constructing the aws autoscaling group from the kops spec
func (b *AutoscalingGroupModelBuilder) Build(c *fi.ModelBuilderContext) error {
	for _, ig := range b.InstanceGroups {
		name := b.AutoscalingGroupName(ig)

		if featureflag.SpotinstHybrid.Enabled() {
			if spotinstmodel.HybridInstanceGroup(ig) {
				klog.V(2).Infof("Skipping instance group: %q", name)
				continue
			}
		}

		// @check if his instancegroup is backed by a fleet and override with a launch template
		task, err := func() (fi.Task, error) {
			switch UseLaunchTemplate(ig) {
			case true:
				return b.buildLaunchTemplateTask(c, name, ig)
			default:
				return b.buildLaunchConfigurationTask(c, name, ig)
			}
		}()
		if err != nil {
			return err
		}
		c.AddTask(task)

		// @step: now lets build the autoscaling group task
		tsk, err := b.buildAutoScalingGroupTask(c, name, ig)
		if err != nil {
			return err
		}
		switch UseLaunchTemplate(ig) {
		case true:
			tsk.LaunchTemplate = task.(*awstasks.LaunchTemplate)
		default:
			tsk.LaunchConfiguration = task.(*awstasks.LaunchConfiguration)
		}
		c.AddTask(tsk)

	}

	return nil
}

// buildLaunchTemplateTask is responsible for creating the template task into the aws model
func (b *AutoscalingGroupModelBuilder) buildLaunchTemplateTask(c *fi.ModelBuilderContext, name string, ig *kops.InstanceGroup) (*awstasks.LaunchTemplate, error) {
	lc, err := b.buildLaunchConfigurationTask(c, name, ig)
	if err != nil {
		return nil, err
	}

	tags, err := b.CloudTagsForInstanceGroup(ig)
	if err != nil {
		return nil, fmt.Errorf("error building cloud tags: %v", err)
	}

	// @TODO check if there any a better way of doing this .. initially I had a type LaunchTemplate which included
	// LaunchConfiguration as an anonymous field, bit given up the task dependency walker works this caused issues, due
	// to the creation of a implicit dependency
	lt := &awstasks.LaunchTemplate{
		Name:                    fi.String(name),
		Lifecycle:               b.Lifecycle,
		AssociatePublicIP:       lc.AssociatePublicIP,
		BlockDeviceMappings:     lc.BlockDeviceMappings,
		IAMInstanceProfile:      lc.IAMInstanceProfile,
		ImageID:                 lc.ImageID,
		InstanceMonitoring:      lc.InstanceMonitoring,
		InstanceType:            lc.InstanceType,
		RootVolumeOptimization:  lc.RootVolumeOptimization,
		RootVolumeSize:          lc.RootVolumeSize,
		RootVolumeIops:          lc.RootVolumeIops,
		RootVolumeType:          lc.RootVolumeType,
		RootVolumeEncryption:    lc.RootVolumeEncryption,
		SSHKey:                  lc.SSHKey,
		SecurityGroups:          lc.SecurityGroups,
		Tags:                    tags,
		Tenancy:                 lc.Tenancy,
		UserData:                lc.UserData,
		HTTPTokens:              lc.HTTPTokens,
		HTTPPutResponseHopLimit: lc.HTTPPutResponseHopLimit,
	}
	// When using a MixedInstances ASG, AWS requires the SpotPrice be defined on the ASG
	// rather than the LaunchTemplate or else it returns this error:
	//   You cannot use a launch template that is set to request Spot Instances (InstanceMarketOptions)
	//   when you configure an Auto Scaling group with a mixed instances policy.
	if ig.Spec.MixedInstancesPolicy == nil {
		lt.SpotPrice = fi.String(lc.SpotPrice)
	} else {
		lt.SpotPrice = fi.String("")
	}
	if ig.Spec.SpotDurationInMinutes != nil {
		lt.SpotDurationInMinutes = ig.Spec.SpotDurationInMinutes
	}
	if ig.Spec.InstanceInterruptionBehavior != nil {
		lt.InstanceInterruptionBehavior = ig.Spec.InstanceInterruptionBehavior
	}
	if fi.BoolValue(ig.Spec.RootVolumeEncryption) && ig.Spec.RootVolumeEncryptionKey != nil {
		lt.RootVolumeKmsKey = ig.Spec.RootVolumeEncryptionKey
	} else {
		lt.RootVolumeKmsKey = fi.String("")
	}
	if fi.StringValue(lt.RootVolumeType) == ec2.VolumeTypeGp3 {
		if fi.Int32Value(ig.Spec.RootVolumeIops) < 3000 {
			lt.RootVolumeIops = fi.Int64(int64(DefaultVolumeGp3Iops))
		} else {
			lt.RootVolumeIops = fi.Int64(int64(fi.Int32Value(ig.Spec.RootVolumeIops)))
		}
		if fi.Int32Value(ig.Spec.RootVolumeThroughput) < 125 {
			lt.RootVolumeThroughput = fi.Int64(int64(DefaultVolumeGp3Throughput))
		} else {
			lt.RootVolumeThroughput = fi.Int64(int64(fi.Int32Value(ig.Spec.RootVolumeThroughput)))
		}
	}
	return lt, nil
}

// buildLaunchConfigurationTask is responsible for building a launch configuration task into the model
func (b *AutoscalingGroupModelBuilder) buildLaunchConfigurationTask(c *fi.ModelBuilderContext, name string, ig *kops.InstanceGroup) (*awstasks.LaunchConfiguration, error) {
	// @step: lets add the root volume settings
	volumeSize, err := defaults.DefaultInstanceGroupVolumeSize(ig.Spec.Role)
	if err != nil {
		return nil, err
	}
	if fi.Int32Value(ig.Spec.RootVolumeSize) > 0 {
		volumeSize = fi.Int32Value(ig.Spec.RootVolumeSize)
	}

	volumeType := fi.StringValue(ig.Spec.RootVolumeType)
	if volumeType == "" {
		volumeType = DefaultVolumeType
	}

	rootVolumeDeleteOnTermination := DefaultVolumeDeleteOnTermination
	if ig.Spec.RootVolumeDeleteOnTermination != nil {
		rootVolumeDeleteOnTermination = fi.BoolValue(ig.Spec.RootVolumeDeleteOnTermination)
	}

	rootVolumeEncryption := DefaultVolumeEncryption
	if ig.Spec.RootVolumeEncryption != nil {
		rootVolumeEncryption = fi.BoolValue(ig.Spec.RootVolumeEncryption)
	}

	// @step: if required we add the override for the security group for this instancegroup
	sgLink := b.LinkToSecurityGroup(ig.Spec.Role)
	if ig.Spec.SecurityGroupOverride != nil {
		sgName := fmt.Sprintf("%v-%v", fi.StringValue(ig.Spec.SecurityGroupOverride), ig.Spec.Role)
		sgLink = &awstasks.SecurityGroup{
			ID:     ig.Spec.SecurityGroupOverride,
			Name:   &sgName,
			Shared: fi.Bool(true),
		}
	}

	// @step: add the iam instance profile
	link, err := b.LinkToIAMInstanceProfile(ig)
	if err != nil {
		return nil, fmt.Errorf("unable to find IAM profile link for instance group %q: %w", ig.ObjectMeta.Name, err)
	}

	t := &awstasks.LaunchConfiguration{
		Name:                          fi.String(name),
		Lifecycle:                     b.Lifecycle,
		IAMInstanceProfile:            link,
		ImageID:                       fi.String(ig.Spec.Image),
		InstanceMonitoring:            ig.Spec.DetailedInstanceMonitoring,
		InstanceType:                  fi.String(strings.Split(ig.Spec.MachineType, ",")[0]),
		RootVolumeDeleteOnTermination: fi.Bool(rootVolumeDeleteOnTermination),
		RootVolumeOptimization:        ig.Spec.RootVolumeOptimization,
		RootVolumeSize:                fi.Int64(int64(volumeSize)),
		RootVolumeType:                fi.String(volumeType),
		RootVolumeEncryption:          fi.Bool(rootVolumeEncryption),
		SecurityGroups:                []*awstasks.SecurityGroup{sgLink},
	}

	t.HTTPTokens = fi.String("optional")
	if ig.Spec.InstanceMetadata != nil && ig.Spec.InstanceMetadata.HTTPTokens != nil {
		t.HTTPTokens = ig.Spec.InstanceMetadata.HTTPTokens
	}
	t.HTTPPutResponseHopLimit = fi.Int64(1)
	if ig.Spec.InstanceMetadata != nil && ig.Spec.InstanceMetadata.HTTPPutResponseHopLimit != nil {
		t.HTTPPutResponseHopLimit = ig.Spec.InstanceMetadata.HTTPPutResponseHopLimit
	}

	if ig.Spec.Role == kops.InstanceGroupRoleMaster &&
		b.APILoadBalancerClass() == kops.LoadBalancerClassNetwork {
		for _, id := range b.Cluster.Spec.API.LoadBalancer.AdditionalSecurityGroups {
			sgTask := &awstasks.SecurityGroup{
				ID:        fi.String(id),
				Lifecycle: b.SecurityLifecycle,
				Name:      fi.String("nlb-" + id),
				Shared:    fi.Bool(true),
			}
			if err := c.EnsureTask(sgTask); err != nil {
				return nil, err
			}
			t.SecurityGroups = append(t.SecurityGroups, sgTask)
		}
	}

	if volumeType == ec2.VolumeTypeIo1 || volumeType == ec2.VolumeTypeIo2 {
		if fi.Int32Value(ig.Spec.RootVolumeIops) < 100 {
			t.RootVolumeIops = fi.Int64(int64(DefaultVolumeIonIops))
		} else {
			t.RootVolumeIops = fi.Int64(int64(fi.Int32Value(ig.Spec.RootVolumeIops)))
		}
	}

	if ig.Spec.Tenancy != "" {
		t.Tenancy = fi.String(ig.Spec.Tenancy)
	}

	// @step: add any additional security groups to the instancegroup
	for _, id := range ig.Spec.AdditionalSecurityGroups {
		sgTask := &awstasks.SecurityGroup{
			ID:        fi.String(id),
			Lifecycle: b.SecurityLifecycle,
			Name:      fi.String(id),
			Shared:    fi.Bool(true),
		}
		if err := c.EnsureTask(sgTask); err != nil {
			return nil, err
		}
		t.SecurityGroups = append(t.SecurityGroups, sgTask)
	}

	// @step: add any additional block devices to the launch configuration
	for i := range ig.Spec.Volumes {
		x := &ig.Spec.Volumes[i]
		if x.Type == "" {
			x.Type = DefaultVolumeType
		}
		if x.Type == ec2.VolumeTypeIo1 || x.Type == ec2.VolumeTypeIo2 {
			if x.Iops == nil {
				x.Iops = fi.Int64(DefaultVolumeIonIops)
			}
		} else if x.Type == ec2.VolumeTypeGp3 {
			if x.Iops == nil {
				x.Iops = fi.Int64(DefaultVolumeGp3Iops)
			}
			if x.Throughput == nil {
				x.Throughput = fi.Int64(DefaultVolumeGp3Throughput)
			}
		} else {
			x.Iops = nil
		}
		deleteOnTermination := DefaultVolumeDeleteOnTermination
		if x.DeleteOnTermination != nil {
			deleteOnTermination = fi.BoolValue(x.DeleteOnTermination)
		}
		encryption := DefaultVolumeEncryption
		if x.Encrypted != nil {
			encryption = fi.BoolValue(x.Encrypted)
		}
		t.BlockDeviceMappings = append(t.BlockDeviceMappings, &awstasks.BlockDeviceMapping{
			DeviceName:             fi.String(x.Device),
			EbsDeleteOnTermination: fi.Bool(deleteOnTermination),
			EbsEncrypted:           fi.Bool(encryption),
			EbsKmsKey:              x.Key,
			EbsVolumeIops:          x.Iops,
			EbsVolumeSize:          fi.Int64(x.Size),
			EbsVolumeThroughput:    x.Throughput,
			EbsVolumeType:          fi.String(x.Type),
		})
	}

	if b.AWSModelContext.UseSSHKey() {
		if t.SSHKey, err = b.LinkToSSHKey(); err != nil {
			return nil, err
		}
	}

	// @step: add the instancegroup userdata
	if t.UserData, err = b.BootstrapScriptBuilder.ResourceNodeUp(c, ig); err != nil {
		return nil, err
	}

	// @step: set up instance spot pricing
	if fi.StringValue(ig.Spec.MaxPrice) != "" {
		spotPrice := fi.StringValue(ig.Spec.MaxPrice)
		t.SpotPrice = spotPrice
	}

	// @step: check the subnets are ok and pull together an array for us
	subnets, err := b.GatherSubnets(ig)
	if err != nil {
		return nil, err
	}

	// @step: check if we can add an public ip to this subnet
	switch subnets[0].Type {
	case kops.SubnetTypePublic, kops.SubnetTypeUtility:
		t.AssociatePublicIP = fi.Bool(true)
		if ig.Spec.AssociatePublicIP != nil {
			t.AssociatePublicIP = ig.Spec.AssociatePublicIP
		}
	case kops.SubnetTypePrivate:
		t.AssociatePublicIP = fi.Bool(false)
	}

	return t, nil
}

// buildAutoscalingGroupTask is responsible for building the autoscaling task into the model
func (b *AutoscalingGroupModelBuilder) buildAutoScalingGroupTask(c *fi.ModelBuilderContext, name string, ig *kops.InstanceGroup) (*awstasks.AutoscalingGroup, error) {

	t := &awstasks.AutoscalingGroup{
		Name:      fi.String(name),
		Lifecycle: b.Lifecycle,

		Granularity: fi.String("1Minute"),
		Metrics: []string{
			"GroupDesiredCapacity",
			"GroupInServiceInstances",
			"GroupMaxSize",
			"GroupMinSize",
			"GroupPendingInstances",
			"GroupStandbyInstances",
			"GroupTerminatingInstances",
			"GroupTotalInstances",
		},
	}

	minSize := fi.Int64(1)
	maxSize := fi.Int64(1)
	if ig.Spec.MinSize != nil {
		minSize = fi.Int64(int64(*ig.Spec.MinSize))
	} else if ig.Spec.Role == kops.InstanceGroupRoleNode {
		minSize = fi.Int64(2)
	}
	if ig.Spec.MaxSize != nil {
		maxSize = fi.Int64(int64(*ig.Spec.MaxSize))
	} else if ig.Spec.Role == kops.InstanceGroupRoleNode {
		maxSize = fi.Int64(2)
	}

	t.MinSize = minSize
	t.MaxSize = maxSize

	subnets, err := b.GatherSubnets(ig)
	if err != nil {
		return nil, err
	}
	if len(subnets) == 0 {
		return nil, fmt.Errorf("could not determine any subnets for InstanceGroup %q; subnets was %s", ig.ObjectMeta.Name, ig.Spec.Subnets)
	}
	for _, subnet := range subnets {
		t.Subnets = append(t.Subnets, b.LinkToSubnet(subnet))
	}

	tags, err := b.CloudTagsForInstanceGroup(ig)
	if err != nil {
		return nil, fmt.Errorf("error building cloud tags: %v", err)
	}
	t.Tags = tags

	processes := []string{}
	processes = append(processes, ig.Spec.SuspendProcesses...)
	t.SuspendProcesses = &processes

	t.InstanceProtection = ig.Spec.InstanceProtection

	t.LoadBalancers = []*awstasks.ClassicLoadBalancer{}
	t.TargetGroups = []*awstasks.TargetGroup{}

	// Spotinst handles load balancer attachments internally, so there's no
	// need to create separate attachments for both managed (+Spotinst) and
	// hybrid (+SpotinstHybrid) instance groups.
	if !featureflag.Spotinst.Enabled() ||
		(featureflag.SpotinstHybrid.Enabled() && !spotinstmodel.HybridInstanceGroup(ig)) {
		if b.UseLoadBalancerForAPI() && ig.Spec.Role == kops.InstanceGroupRoleMaster {
			if b.UseNetworkLoadBalancer() {
				t.TargetGroups = append(t.TargetGroups, b.LinkToTargetGroup("tcp"))
				if b.Cluster.Spec.API.LoadBalancer.SSLCertificate != "" {
					t.TargetGroups = append(t.TargetGroups, b.LinkToTargetGroup("tls"))
				}
			} else {
				t.LoadBalancers = append(t.LoadBalancers, b.LinkToCLB("api"))
			}
		}

		if ig.Spec.Role == kops.InstanceGroupRoleBastion {
			t.LoadBalancers = append(t.LoadBalancers, b.LinkToCLB("bastion"))
		}
	}

	for _, extLB := range ig.Spec.ExternalLoadBalancers {
		if extLB.LoadBalancerName != nil {
			lb := &awstasks.ClassicLoadBalancer{
				Name:             extLB.LoadBalancerName,
				LoadBalancerName: extLB.LoadBalancerName,
				Shared:           fi.Bool(true),
			}
			t.LoadBalancers = append(t.LoadBalancers, lb)
			c.EnsureTask(lb)
		}

		if extLB.TargetGroupARN != nil {
			targetGroupName, err := awsup.GetTargetGroupNameFromARN(fi.StringValue(extLB.TargetGroupARN))
			if err != nil {
				return nil, err
			}
			tg := &awstasks.TargetGroup{
				Name:   fi.String(name + "-" + targetGroupName),
				ARN:    extLB.TargetGroupARN,
				Shared: fi.Bool(true),
			}
			t.TargetGroups = append(t.TargetGroups, tg)
			c.AddTask(tg)
		}
	}
	sort.Stable(awstasks.OrderLoadBalancersByName(t.LoadBalancers))
	sort.Stable(awstasks.OrderTargetGroupsByName(t.TargetGroups))

	// @step: are we using a mixed instance policy
	if ig.Spec.MixedInstancesPolicy != nil {
		spec := ig.Spec.MixedInstancesPolicy

		t.MixedInstanceOverrides = spec.Instances
		t.MixedOnDemandAboveBase = spec.OnDemandAboveBase
		t.MixedOnDemandAllocationStrategy = spec.OnDemandAllocationStrategy
		t.MixedOnDemandBase = spec.OnDemandBase
		t.MixedSpotAllocationStrategy = spec.SpotAllocationStrategy
		t.MixedSpotInstancePools = spec.SpotInstancePools
		t.MixedSpotMaxPrice = ig.Spec.MaxPrice
	}

	return t, nil
}
