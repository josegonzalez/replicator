package client

import (
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/elsevier-core-engineering/replicator/logging"
	"github.com/elsevier-core-engineering/replicator/replicator/structs"
	nomad "github.com/hashicorp/nomad/api"
	nomadstructs "github.com/hashicorp/nomad/nomad/structs"
)

const (
	deploymentTimeOut = 15 * time.Minute
)

// JobGroupScale scales a particular job group, confirming that the action
// completes successfully.
func (c *nomadClient) JobGroupScale(jobName string, group *structs.GroupScalingPolicy, state *structs.ScalingState) {

	// In order to scale the job, we need information on the current status of the
	// running job from Nomad.
	jobResp, _, err := c.nomad.Jobs().Info(jobName, &nomad.QueryOptions{})

	if err != nil {
		logging.Error("client/job_scaling: unable to determine job info of %v: %v", jobName, err)
		return
	}

	// Use the current task count in order to determine whether or not a scaling
	// event will violate the min/max job policy.
	for i, taskGroup := range jobResp.TaskGroups {
		if group.ScaleDirection == ScalingDirectionOut && *taskGroup.Count >= group.Max ||
			group.ScaleDirection == ScalingDirectionIn && *taskGroup.Count <= group.Min {
			logging.Debug("client/job_scaling: scale %v not permitted due to constraints on job \"%v\" and group \"%v\"",
				group.ScaleDirection, *jobResp.ID, group.GroupName)
			return
		}

		logging.Info("client/job_scaling: scale %v will now be initiated against job \"%v\" and group \"%v\"",
			group.ScaleDirection, jobName, group.GroupName)

		// Depending on the scaling direction decrement/incrament the count;
		// currently replicator only supports addition/subtraction of 1.
		if *taskGroup.Name == group.GroupName && group.ScaleDirection == ScalingDirectionOut {
			metrics.IncrCounter([]string{"job", jobName, group.GroupName, "scale_out"}, 1)
			*jobResp.TaskGroups[i].Count++
			state.ScaleOutRequests++
		}

		if *taskGroup.Name == group.GroupName && group.ScaleDirection == ScalingDirectionIn {
			metrics.IncrCounter([]string{"job", jobName, group.GroupName, "scale_in"}, 1)
			*jobResp.TaskGroups[i].Count--
			state.ScaleInRequests++
		}
	}

	// Submit the job to the Register API endpoint with the altered count number
	// and check that no error is returned.
	resp, _, err := c.nomad.Jobs().Register(jobResp, &nomad.WriteOptions{})

	// Track the scaling submission time.
	state.LastScalingEvent = time.Now()
	if err != nil {
		logging.Error("client/job_scaling: issue submitting job %s for scaling action: %v", jobName, err)
		return
	}

	success := c.scaleConfirmation(resp.EvalID)

	if !success {
		state.FailureCount++

		return
	}

	logging.Info("client/job_scaling: scaling of job \"%v\" and group \"%v\" successfully completed",
		jobName, group.GroupName)
}

// scaleConfirmation takes the EvaluationID from the job registration and checks
// via a timer and blocking queries that the resulting deployment completes
// successfully.
func (c *nomadClient) scaleConfirmation(evalID string) (success bool) {

	eval, _, err := c.nomad.Evaluations().Info(evalID, nil)
	if err != nil {
		logging.Error("client/job_scaling: unable to obtain eval info for %s: %v", evalID, err)
		return
	}

	timeOut := time.After(deploymentTimeOut)
	tick := time.Tick(500 * time.Millisecond)
	depID := eval.DeploymentID
	q := &nomad.QueryOptions{WaitIndex: 1, AllowStale: true}

	for {
		select {
		case <-timeOut:
			logging.Error("client/job_scaling: deployment %s reached timeout %v", depID, deploymentTimeOut)
			return

		case <-tick:
			dep, meta, err := c.nomad.Deployments().Info(depID, q)
			if err != nil {
				logging.Error("client/job_scaling: unable to list Nomad deployment %s: %v", depID, err)
				return
			}

			// Check the LastIndex for an update.
			if meta.LastIndex <= q.WaitIndex {
				continue
			}

			q.WaitIndex = meta.LastIndex

			// Check the deployment status.
			if dep.Status == nomadstructs.DeploymentStatusSuccessful {
				return true
			} else if dep.Status == nomadstructs.DeploymentStatusRunning {
				logging.Debug("client/job_scaling: deployment %s is still in progress", depID)
				continue
			} else {
				return false
			}
		}
	}
}

// IsJobInDeployment checks to see whether the supplied Nomad job is currently
// in the process of a deployment.
func (c *nomadClient) IsJobInDeployment(jobName string) (isRunning bool) {

	resp, _, err := c.nomad.Jobs().LatestDeployment(jobName, nil)

	if err != nil {
		logging.Error("client/job_scaling: unable to list Nomad deployments: %v", err)
		return
	}

	switch resp.Status {
	case nomadstructs.DeploymentStatusRunning:
		return true
	case nomadstructs.DeploymentStatusDescriptionPaused:
		return true
	default:
		return false
	}
}
