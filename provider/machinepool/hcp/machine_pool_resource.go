/*
Copyright (c) 2024 Red Hat, Inc.

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

package hcp

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	sdk "github.com/openshift-online/ocm-sdk-go"
	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	"github.com/terraform-redhat/terraform-provider-rhcs/provider/common"
)

// This is a magic name to trigger special handling for the cluster's default
// machine pool
const defaultNodePoolName = "worker"

var nodePoolNameRE = regexp.MustCompile(
	`^[a-z]([-a-z0-9]*[a-z0-9])?$`,
)

type HcpMachinePoolResource struct {
	collection  *cmv1.ClustersClient
	clusterWait common.ClusterWait
}

var _ resource.ResourceWithConfigure = &HcpMachinePoolResource{}
var _ resource.ResourceWithImportState = &HcpMachinePoolResource{}
var _ resource.ResourceWithConfigValidators = &HcpMachinePoolResource{}

func New() resource.Resource {
	return &HcpMachinePoolResource{}
}

func (r *HcpMachinePoolResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hcp_machine_pool"
}

func (r *HcpMachinePoolResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Machine pool.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Unique identifier of the machine pool.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "Name of the machine pool. Must consist of lower-case alphanumeric characters or '-', start and end with an alphanumeric character. " + common.ValueCannotBeChangedStringDescription,
				Required:    true,
			},
			"cluster": schema.StringAttribute{
				Description: "Identifier of the cluster. " + common.ValueCannotBeChangedStringDescription,
				Required:    true,
			},
			"replicas": schema.Int64Attribute{
				Description: "The number of machines of the pool",
				Optional:    true,
			},
			"autoscaling": schema.SingleNestedAttribute{
				Description: "Basic autoscaling options",
				Attributes:  AutoscalingResource(),
				Required:    true,
			},
			"taints": schema.ListNestedAttribute{
				Description: "Taints for a machine pool. Format should be a comma-separated " +
					"list of 'key=value'. This list will overwrite any modifications " +
					"made to node taints on an ongoing basis.\n",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"key": schema.StringAttribute{
							Description: "Taints key",
							Required:    true,
						},
						"value": schema.StringAttribute{
							Description: "Taints value",
							Required:    true,
						},
						"schedule_type": schema.StringAttribute{
							Description: "Taints schedule type",
							Required:    true,
							Validators: []validator.String{
								stringvalidator.OneOf("NoSchedule", "PreferNoSchedule", "NoExecute"),
							},
						},
					},
				},
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
				},
				Optional: true,
			},
			"labels": schema.MapAttribute{
				Description: "Labels for the machine pool. Format should be a comma-separated list of 'key = value'." +
					" This list will overwrite any modifications made to node labels on an ongoing basis.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.Map{
					mapvalidator.SizeAtLeast(1),
				},
			},
			"availability_zone": schema.StringAttribute{
				Description: "Select the availability zone in which to create a single AZ machine pool for a multi-AZ cluster. " + common.ValueCannotBeChangedStringDescription,
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"subnet_id": schema.StringAttribute{
				Description: "Select the subnet in which to create a single AZ machine pool for BYO-VPC cluster. " + common.ValueCannotBeChangedStringDescription,
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			// "status": schema.SingleNestedAttribute{
			// 	Description: "HCP replica status",
			// 	Attributes:  NodePoolStatusResource(),
			// 	Computed:    true,
			// },
			"aws_node_pool": schema.SingleNestedAttribute{
				Description: "AWS settings for node pool",
				Attributes:  AwsNodePoolResource(),
				Optional:    true,
			},
			// "tuning_configs": schema.ListAttribute{
			// 	Description: "A list of tuning configs attached to the replica.",
			// 	ElementType: types.StringType,
			// 	Optional:    true,
			// },
			"auto_repair": schema.BoolAttribute{
				Description: "Indicates use of autor repair for replica",
				Optional:    true,
				Computed:    true,
			},
		},
	}
}

func (r *HcpMachinePoolResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		//resourcevalidator.Conflicting(path.MatchRoot("availability_zone"), path.MatchRoot("subnet_id")),
		// resourcevalidator.RequiredTogether(path.MatchRoot("min_replicas"), path.MatchRoot("max_replicas")),
		// resourcevalidator.Conflicting(path.MatchRoot("replicas"), path.MatchRoot("min_replicas")),
		// resourcevalidator.Conflicting(path.MatchRoot("replicas"), path.MatchRoot("max_replicas")),
	}
}

func (r *HcpMachinePoolResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	connection, ok := req.ProviderData.(*sdk.Connection)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *sdk.Connaction, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}

	r.collection = connection.ClustersMgmt().V1().Clusters()
	r.clusterWait = common.NewClusterWait(r.collection)
}

func (r *HcpMachinePoolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Get the plan:
	plan := &HcpMachinePoolState{}
	diags := req.Plan.Get(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	nodePoolName := plan.Name.ValueString()
	if !nodePoolNameRE.MatchString(nodePoolName) {
		resp.Diagnostics.AddError(
			"Cannot create machine pool: ",
			fmt.Sprintf("Cannot create machine pool for cluster '%s' with name '%s'. Expected a valid value for 'name' matching %s",
				plan.Cluster.ValueString(), plan.Name.ValueString(), nodePoolNameRE,
			),
		)
		return
	}

	// Wait till the cluster is ready:
	err := r.clusterWait.WaitForClusterToBeReady(ctx, plan.Cluster.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Cannot poll cluster state",
			fmt.Sprintf(
				"Cannot poll state of cluster with identifier '%s': %v",
				plan.Cluster.ValueString(), err,
			),
		)
		return
	}

	// The default machine pool is created automatically when the cluster is created.
	// We want to import it instead of creating it.
	if strings.HasPrefix(nodePoolName, defaultNodePoolName) {
		r.magicImport(ctx, plan, resp)
		return
	}

	// Create the machine pool:
	resource := r.collection.Cluster(plan.Cluster.ValueString())
	builder := cmv1.NewNodePool().ID(plan.ID.ValueString())
	builder.ID(plan.Name.ValueString())

	if plan.AWSNodePool != nil {
		awsNodePoolBuilder := cmv1.NewAWSNodePool()
		awsNodePoolBuilder.InstanceType(plan.AWSNodePool.InstanceType.ValueString())
	}

	if !common.IsStringAttributeUnknownOrEmpty(plan.AvailabilityZone) {
		builder.AvailabilityZone(plan.AvailabilityZone.ValueString())
	}
	if !common.IsStringAttributeUnknownOrEmpty(plan.SubnetID) {
		builder.Subnet(plan.SubnetID.ValueString())
	}

	autoscalingEnabled := false
	computeNodeEnabled := false
	autoscalingEnabled, errMsg := getAutoscaling(plan, builder)
	if errMsg != "" {
		resp.Diagnostics.AddError(
			"Cannot build machine pool",
			fmt.Sprintf(
				"Cannot build machine pool for cluster '%s, %s'", plan.Cluster.ValueString(), errMsg,
			),
		)
		return
	}

	if common.HasValue(plan.Replicas) {
		computeNodeEnabled = true
		builder.Replicas(int(plan.Replicas.ValueInt64()))
	}
	if (!autoscalingEnabled && !computeNodeEnabled) || (autoscalingEnabled && computeNodeEnabled) {
		resp.Diagnostics.AddError(
			"Cannot build machine pool",
			fmt.Sprintf(
				"Cannot build machine pool for cluster '%s', please provide a value for either the 'replicas' or 'autoscaling.enabled' parameter. It is mandatory to include at least one of these parameters in the resource plan.",
				plan.Cluster.ValueString(),
			),
		)
		return
	}

	if plan.Taints != nil && len(plan.Taints) > 0 {
		var taintBuilders []*cmv1.TaintBuilder
		for _, taint := range plan.Taints {
			taintBuilders = append(taintBuilders, cmv1.NewTaint().
				Key(taint.Key.ValueString()).
				Value(taint.Value.ValueString()).
				Effect(taint.ScheduleType.ValueString()))
		}
		builder.Taints(taintBuilders...)
	}

	if common.HasValue(plan.Labels) {
		labels := map[string]string{}
		for k, v := range plan.Labels.Elements() {
			labels[k] = v.(types.String).ValueString()
		}
		builder.Labels(labels)
	}

	object, err := builder.Build()
	if err != nil {
		resp.Diagnostics.AddError(
			"Cannot build machine pool",
			fmt.Sprintf(
				"Cannot build machine pool for cluster '%s': %v",
				plan.Cluster.ValueString(), err,
			),
		)
		return
	}

	collection := resource.NodePools()
	add, err := collection.Add().Body(object).SendContext(ctx)
	if err != nil {
		resp.Diagnostics.AddError(
			"Cannot create machine pool",
			fmt.Sprintf(
				"Cannot create machine pool for cluster '%s': %v",
				plan.Cluster.ValueString(), err,
			),
		)
		return
	}
	object = add.Body()

	// Save the state:
	err = populateState(object, plan)
	if err != nil {
		resp.Diagnostics.AddError(
			"Can't populate machine pool state",
			fmt.Sprintf(
				"Received error %v", err,
			),
		)
		return
	}
	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

// This handles the "magic" import of the default machine pool, allowing the
// user to include it in their config w/o having to specifically `terraform
// import` it.
func (r *HcpMachinePoolResource) magicImport(ctx context.Context, plan *HcpMachinePoolState, resp *resource.CreateResponse) {
	nodePoolName := plan.Name.ValueString()
	state := &HcpMachinePoolState{
		ID:      types.StringValue(nodePoolName),
		Cluster: plan.Cluster,
		Name:    types.StringValue(nodePoolName),
	}
	plan.ID = types.StringValue(nodePoolName)

	notFound, diags := readState(ctx, state, r.collection)
	if notFound {
		// We disallow creating a machine pool with the default name. This
		// case can only happen if the default machine pool was deleted and
		// the user tries to recreate it.
		diags.AddError(
			"Can't create machine pool",
			fmt.Sprintf(
				"Can't create machine pool for cluster '%s': "+
					"the default machine pool '%s' was deleted and a new machine pool with that name may not be created. "+
					"Please use a different name.",
				plan.Cluster.ValueString(),
				nodePoolName,
			),
		)
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	diags = r.doUpdate(ctx, state, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *HcpMachinePoolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Get the current state:
	state := &HcpMachinePoolState{}
	diags := req.State.Get(ctx, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	notFound, diags := readState(ctx, state, r.collection)
	if notFound {
		// If we can't find the machine pool, it was deleted. Remove if from the
		// state and don't return an error so the TF apply() will automatically
		// recreate it.
		tflog.Warn(ctx, fmt.Sprintf("machine pool (%s) of cluster (%s) not found, removing from state",
			state.ID.ValueString(), state.Cluster.ValueString(),
		))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func readState(ctx context.Context, state *HcpMachinePoolState, collection *cmv1.ClustersClient) (poolNotFound bool, diags diag.Diagnostics) {
	diags = diag.Diagnostics{}

	resource := collection.Cluster(state.Cluster.ValueString()).
		NodePools().
		NodePool(state.ID.ValueString())
	get, err := resource.Get().SendContext(ctx)
	if err != nil && get.Status() == http.StatusNotFound {
		poolNotFound = true
		return
	} else if err != nil {
		diags.AddError(
			"Failed to fetch machine pool",
			fmt.Sprintf(
				"Failed to fetch machine pool with identifier %s for cluster %s. Response code: %v",
				state.ID.ValueString(), state.Cluster.ValueString(), get.Status(),
			),
		)
		return
	}

	object := get.Body()
	err = populateState(object, state)
	if err != nil {
		diags.AddError(
			"Can't populate machine pool state",
			fmt.Sprintf(
				"Received error %v", err,
			),
		)
		return
	}
	return
}

func validateNoImmutableAttChange(state, plan *HcpMachinePoolState) diag.Diagnostics {
	diags := diag.Diagnostics{}
	validateStateAndPlanEquals(state.Cluster, plan.Cluster, "cluster", &diags)
	validateStateAndPlanEquals(state.Name, plan.Name, "name", &diags)
	if state.AWSNodePool != nil && plan.AWSNodePool != nil {
		validateStateAndPlanEquals(state.AWSNodePool.InstanceType, plan.AWSNodePool.InstanceType, "aws_node_pool.instance_type", &diags)
	}
	validateStateAndPlanEquals(state.AvailabilityZone, plan.AvailabilityZone, "availability_zone", &diags)
	validateStateAndPlanEquals(state.SubnetID, plan.SubnetID, "subnet_id", &diags)
	return diags
}

func validateStateAndPlanEquals(stateAttr attr.Value, planAttr attr.Value, attrName string, diags *diag.Diagnostics) {
	// Its possible to have here unknown attributes
	// Relevant only for optional computed attributes in resource create
	// Check this because this function also used in "magicImport" function
	if planAttr.IsUnknown() {
		return
	}
	common.ValidateStateAndPlanEquals(stateAttr, planAttr, attrName, diags)
}

func (r *HcpMachinePoolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Get the state:
	state := &HcpMachinePoolState{}
	diags := req.State.Get(ctx, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Get the plan:
	plan := &HcpMachinePoolState{}
	diags = req.Plan.Get(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	diags = r.doUpdate(ctx, state, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Save the state:
	diags = resp.State.Set(ctx, state)
	resp.Diagnostics.Append(diags...)
}

func (r *HcpMachinePoolResource) doUpdate(ctx context.Context, state *HcpMachinePoolState, plan *HcpMachinePoolState) diag.Diagnostics {
	//assert no changes on specific attributes
	diags := validateNoImmutableAttChange(state, plan)
	if diags.HasError() {
		return diags
	}

	resource := r.collection.Cluster(state.Cluster.ValueString()).
		NodePools().
		NodePool(state.ID.ValueString())
	_, err := resource.Get().SendContext(ctx)

	if err != nil {
		diags.AddError(
			"Cannot find machine pool",
			fmt.Sprintf(
				"Cannot find machine pool with identifier '%s' for "+
					"cluster '%s': %v",
				state.ID.ValueString(), state.Cluster.ValueString(), err,
			),
		)
		return diags
	}

	npBuilder := cmv1.NewNodePool().ID(state.ID.ValueString())

	if state.AWSNodePool != nil && plan.AWSNodePool != nil {
		_, ok := common.ShouldPatchString(state.AWSNodePool.InstanceType, plan.AWSNodePool.InstanceType)
		if ok {
			diags.AddError(
				"Cannot update machine pool",
				fmt.Sprintf(
					"Cannot update machine pool for cluster '%s', machine type cannot be updated",
					state.Cluster.ValueString(),
				),
			)
			return diags
		}
	}

	computeNodesEnabled := false
	autoscalingEnabled := false

	if common.HasValue(plan.Replicas) {
		computeNodesEnabled = true
		npBuilder.Replicas(int(plan.Replicas.ValueInt64()))
	}

	autoscalingEnabled, errMsg := getAutoscaling(plan, npBuilder)
	if errMsg != "" {
		diags.AddError(
			"Cannot update machine pool",
			fmt.Sprintf(
				"Cannot update machine pool for cluster '%s, %s ", state.Cluster.ValueString(), errMsg,
			),
		)
		return diags
	}

	if (autoscalingEnabled && computeNodesEnabled) || (!autoscalingEnabled && !computeNodesEnabled) {
		diags.AddError(
			"Cannot update machine pool",
			fmt.Sprintf(
				"Cannot update machine pool for cluster '%s: either replicas should be set or autoscaling enabled", state.Cluster.ValueString(),
			),
		)
		return diags
	}

	patchLabels, shouldPatchLabels := common.ShouldPatchMap(state.Labels, plan.Labels)
	if shouldPatchLabels {
		labels := map[string]string{}
		for k, v := range patchLabels.Elements() {
			labels[k] = v.(types.String).ValueString()
		}
		npBuilder.Labels(labels)
	}

	if shouldPatchTaints(state.Taints, plan.Taints) {
		var taintBuilders []*cmv1.TaintBuilder
		for _, taint := range plan.Taints {
			taintBuilders = append(taintBuilders, cmv1.NewTaint().
				Key(taint.Key.ValueString()).
				Value(taint.Value.ValueString()).
				Effect(taint.ScheduleType.ValueString()))
		}
		npBuilder.Taints(taintBuilders...)
	}

	nodePool, err := npBuilder.Build()
	if err != nil {
		diags.AddError(
			"Cannot update machine pool",
			fmt.Sprintf(
				"Cannot update machine pool for cluster '%s: %v ", state.Cluster.ValueString(), err,
			),
		)
		return diags
	}
	update, err := r.collection.Cluster(state.Cluster.ValueString()).
		NodePools().
		NodePool(state.ID.ValueString()).Update().Body(nodePool).SendContext(ctx)
	if err != nil {
		diags.AddError(
			"Failed to update machine pool",
			fmt.Sprintf(
				"Failed to update machine pool '%s'  on cluster '%s': %v",
				state.ID.ValueString(), state.Cluster.ValueString(), err,
			),
		)
		return diags
	}

	object := update.Body()

	// update the autoscaling enabled with the plan value (important for nil and false cases)
	state.AutoScaling.Enabled = plan.AutoScaling.Enabled
	// update the Replicas with the plan value (important for nil and zero value cases)
	state.Replicas = plan.Replicas

	// Save the state:
	err = populateState(object, state)
	if err != nil {
		diags.AddError(
			"Can't populate machine pool state",
			fmt.Sprintf(
				"Received error %v", err,
			),
		)
		return diags
	}
	return diags
}

func getAutoscaling(state *HcpMachinePoolState, mpBuilder *cmv1.NodePoolBuilder) (
	autoscalingEnabled bool, errMsg string) {
	autoscalingEnabled = false
	if common.HasValue(state.AutoScaling.Enabled) &&
		state.AutoScaling.Enabled.ValueBool() {
		autoscalingEnabled = true

		autoscaling := cmv1.NewNodePoolAutoscaling()
		if common.HasValue(state.AutoScaling.MaxReplicas) {
			autoscaling.MaxReplica(int(state.AutoScaling.MaxReplicas.ValueInt64()))
		} else {
			return false, "when enabling autoscaling, should set value for maxReplicas"
		}
		if common.HasValue(state.AutoScaling.MinReplicas) {
			autoscaling.MinReplica(int(state.AutoScaling.MinReplicas.ValueInt64()))
		} else {
			return false, "when enabling autoscaling, should set value for minReplicas"
		}
		if !autoscaling.Empty() {
			mpBuilder.Autoscaling(autoscaling)
		}
	} else {
		if common.HasValue(state.AutoScaling.MaxReplicas) ||
			common.HasValue(state.AutoScaling.MinReplicas) {
			return false, "when disabling autoscaling, cannot set min_replicas and/or max_replicas"
		}
	}

	return autoscalingEnabled, ""
}

func (r *HcpMachinePoolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Get the state:
	state := &HcpMachinePoolState{}
	diags := req.State.Get(ctx, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Send the request to delete the machine pool:
	resource := r.collection.Cluster(state.Cluster.ValueString()).
		NodePools().
		NodePool(state.ID.ValueString())
	_, err := resource.Delete().SendContext(ctx)
	if err != nil {
		// We can't delete the pool, see if it's the last one:
		numPools, err2 := r.countPools(ctx, state.Cluster.ValueString())
		if numPools == 1 && err2 == nil {
			// It's the last one, issue warning instead of error
			resp.Diagnostics.AddWarning(
				"Cannot delete machine pool",
				fmt.Sprintf(
					"Cannot delete the last machine pool for cluster '%s'. "+
						"ROSA Classic clusters must have at least one machine pool. "+
						"It is being removed from the Terraform state only. "+
						"To resume managing this machine pool, import it again. "+
						"It will be automatically deleted when the cluster is deleted.",
					state.Cluster.ValueString(),
				),
			)
			// No return, we want to remove the state
		} else {
			// Wasn't the last one, return error
			resp.Diagnostics.AddError(
				"Cannot delete machine pool",
				fmt.Sprintf(
					"Cannot delete machine pool with identifier '%s' for "+
						"cluster '%s': %v",
					state.ID.ValueString(), state.Cluster.ValueString(), err,
				),
			)
			return
		}
	}

	// Remove the state:
	resp.State.RemoveResource(ctx)
}

// countPools returns the number of machine pools in the given cluster
func (r *HcpMachinePoolResource) countPools(ctx context.Context, clusterID string) (int, error) {
	resource := r.collection.Cluster(clusterID).MachinePools()
	resp, err := resource.List().SendContext(ctx)
	if err != nil {
		return 0, err
	}
	return resp.Size(), nil
}

func (r *HcpMachinePoolResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// To import a machine pool, we need to know the cluster ID and the machine pool ID
	fields := strings.Split(req.ID, ",")
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		resp.Diagnostics.AddError(
			"Invalid import identifier",
			"Machine pool to import should be specified as <cluster_id>,<machine_pool_id>",
		)
		return
	}
	clusterID := fields[0]
	nodePoolId := fields[1]
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster"), clusterID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), nodePoolId)...)
}

// populateState copies the data from the API object to the Terraform state.
func populateState(object *cmv1.NodePool, state *HcpMachinePoolState) error {
	state.ID = types.StringValue(object.ID())
	state.Name = types.StringValue(object.ID())

	if awsNodePool, ok := object.GetAWSNodePool(); ok {
		if state.AWSNodePool == nil {
			state.AWSNodePool = new(AWSNodePool)
		}
		if instanceType, ok := awsNodePool.GetInstanceType(); ok {
			state.AWSNodePool.InstanceType = types.StringValue(instanceType)
		}
	}

	autoscaling, ok := object.GetAutoscaling()
	if ok {
		var minReplicas, maxReplicas int
		state.AutoScaling.Enabled = types.BoolValue(true)
		minReplicas, ok = autoscaling.GetMinReplica()
		if ok {
			state.AutoScaling.MinReplicas = types.Int64Value(int64(minReplicas))
		}
		maxReplicas, ok = autoscaling.GetMaxReplica()
		if ok {
			state.AutoScaling.MaxReplicas = types.Int64Value(int64(maxReplicas))
		}
	} else {
		state.AutoScaling.MaxReplicas = types.Int64Null()
		state.AutoScaling.MinReplicas = types.Int64Null()
	}

	if replicas, ok := object.GetReplicas(); ok {
		state.Replicas = types.Int64Value(int64(replicas))
	}

	taints := object.Taints()
	if len(taints) > 0 {
		state.Taints = make([]Taints, len(taints))
		for i, taint := range taints {
			state.Taints[i] = Taints{
				Key:          types.StringValue(taint.Key()),
				Value:        types.StringValue(taint.Value()),
				ScheduleType: types.StringValue(taint.Effect()),
			}
		}
	} else {
		state.Taints = nil
	}

	labels := object.Labels()
	if len(labels) > 0 {
		// XXX: We should be checking error here, but we don't have a way to return the error
		state.Labels, _ = common.ConvertStringMapToMapType(labels)
	} else {
		state.Labels = types.MapNull(types.StringType)
	}

	state.SubnetID = types.StringValue(object.Subnet())
	state.AvailabilityZone = types.StringValue(object.AvailabilityZone())
	return nil
}

func shouldPatchTaints(a, b []Taints) bool {
	if (a == nil && b != nil) || (a != nil && b == nil) {
		return true
	}
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if !a[i].Key.Equal(b[i].Key) || !a[i].Value.Equal(b[i].Value) || !a[i].ScheduleType.Equal(b[i].ScheduleType) {
			return true
		}
	}
	return false
}